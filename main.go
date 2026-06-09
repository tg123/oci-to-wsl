package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/urfave/cli/v3"
	"golang.org/x/term"

	"github.com/tg123/oci-to-wsl/config"
	"github.com/tg123/oci-to-wsl/registry"
	"github.com/tg123/oci-to-wsl/wsl"
)

func main() {
	cmd := &cli.Command{
		Name:  "oci-to-wsl",
		Usage: "load an OCI container image into a WSL distribution",
		Description: `Pull an OCI image from any container registry and import it as a
WSL distribution in one command.

If the image already exists in the local Docker daemon it is loaded from there
instead of being pulled from the registry. Set OCI_TO_WSL_NO_LOCAL=1 to disable
this and always pull from the registry.

Examples:
  # Import directly from Docker Hub (uses the local docker daemon if present)
  oci-to-wsl --image ubuntu:22.04 --name my-ubuntu

  # Import from Azure Container Registry (browser login triggered automatically)
  oci-to-wsl --image myacr.azurecr.io/myimage:latest --name myimage

  # Use a YAML profile
  oci-to-wsl --profile ubuntu.yaml

  # Read a YAML profile from stdin or fetch it over the network
  cat ubuntu.yaml | oci-to-wsl --profile -
  oci-to-wsl --profile https://example.com/ubuntu.yaml

  # Also download a URL profile's relative 'files' sources from the same URL
  OCI_TO_WSL_PROFILE_FOLLOW_URL=1 oci-to-wsl --profile https://example.com/ubuntu.yaml

  # Save the rootfs tar for a non-host platform (save-tar mode only)
  OCI_TO_WSL_PLATFORM=linux/arm64 oci-to-wsl --image ubuntu:22.04 --save-tar ubuntu-arm64.tar

  # Save the rootfs tar without applying the profile's 'copies' / 'deletes'
  OCI_TO_WSL_NO_TAR_MODS=1 oci-to-wsl --profile ubuntu.yaml --save-tar ubuntu.tar

  # Record registry credentials in ~/.docker/config.json without needing docker
  oci-to-wsl dockerlogin ghcr.io --username alice`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "profile",
				Usage: "path to a YAML profile file, '-' for stdin, or an http(s):// URL (overrides other flags when set)",
			},
			&cli.StringFlag{
				Name:  "image",
				Usage: "OCI image reference, e.g. ubuntu:22.04 or myacr.azurecr.io/myimage:latest",
			},
			&cli.StringFlag{
				Name:  "name",
				Usage: "WSL distribution name to create",
			},
			&cli.StringFlag{
				Name:  "dir",
				Usage: "directory to store the WSL virtual disk (default: ./<name>)",
			},
			&cli.StringFlag{
				Name:  "save-tar",
				Usage: "write the exported rootfs tar to this path and skip 'wsl --import' (useful on non-Windows hosts). Profile 'copies' and 'deletes' are still applied to the tar; set OCI_TO_WSL_NO_TAR_MODS=1 to skip them. Set OCI_TO_WSL_PLATFORM=os/arch (e.g. linux/arm64) to override the image platform; this is only honored in save-tar mode.",
			},
			&cli.StringFlag{
				Name:  "loglevel",
				Value: "info",
				Usage: "logging verbosity: debug, info, warn, or error",
			},
		},
		Before:   setupLogging,
		Action:   action,
		Commands: []*cli.Command{dockerLoginCommand()},
	}

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func setupLogging(ctx context.Context, cmd *cli.Command) (context.Context, error) {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(cmd.String("loglevel"))) {
	case "", "info":
		lvl = slog.LevelInfo
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return ctx, fmt.Errorf("invalid --loglevel %q (expected one of: debug, info, warn, error)", cmd.String("loglevel"))
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})))
	return ctx, nil
}

func action(_ context.Context, cmd *cli.Command) error {
	profilePath := cmd.String("profile")
	imageName := cmd.String("image")
	distroName := cmd.String("name")
	installDir := cmd.String("dir")
	saveTar := cmd.String("save-tar")

	// Build a Profile from the YAML file or the CLI flags.
	var profile *config.Profile
	if profilePath != "" {
		var err error
		profile, err = config.LoadProfile(profilePath)
		if err != nil {
			return fmt.Errorf("loading profile: %w", err)
		}
	} else {
		if imageName == "" {
			_ = cli.ShowAppHelp(cmd)
			return fmt.Errorf("provide --profile, or --image (and --name unless --save-tar is set)")
		}
		if distroName == "" && saveTar == "" {
			_ = cli.ShowAppHelp(cmd)
			return fmt.Errorf("--name is required unless --save-tar is set")
		}
		profile = &config.Profile{
			Name:       distroName,
			Image:      imageName,
			InstallDir: installDir,
		}
	}

	// Any CLI flag explicitly set on the command line overrides the matching
	// profile field. This applies uniformly to every flag.
	if cmd.IsSet("image") {
		profile.Image = imageName
	}
	if cmd.IsSet("name") {
		profile.Name = distroName
	}
	if cmd.IsSet("dir") {
		profile.InstallDir = installDir
	}

	return loadProfile(profile, saveTar)
}

func loadProfile(profile *config.Profile, saveTar string) error {
	if err := profile.Validate(); err != nil {
		return fmt.Errorf("profile: %w", err)
	}
	if saveTar == "" && profile.Name == "" {
		return fmt.Errorf("profile: 'name' is required")
	}
	if saveTar == "" {
		if profile.InstallDir == "" {
			profile.InstallDir = filepath.Join(".", profile.Name)
		}
		if err := os.MkdirAll(profile.InstallDir, 0700); err != nil {
			return fmt.Errorf("creating install directory %q: %w", profile.InstallDir, err)
		}
	}

	// Decide tar destination + cleanup policy.
	var tarPath string
	var cleanup bool
	if saveTar != "" {
		tarPath = saveTar
		cleanup = false
	} else {
		tarPath = filepath.Join(os.TempDir(), profile.Name+"-rootfs.tar")
		cleanup = true
	}

	tarFile, err := os.Create(tarPath)
	if err != nil {
		return fmt.Errorf("creating tar file %q: %w", tarPath, err)
	}
	defer func() {
		_ = tarFile.Close()
		if cleanup {
			_ = os.Remove(tarPath)
		}
	}()

	// The image platform can only be overridden in save-tar mode (via the
	// OCI_TO_WSL_PLATFORM env var). In WSL-import mode the platform is
	// always the host's: importing an arm rootfs into an x86 WSL (or vice
	// versa) does not work.
	var platform string
	if saveTar != "" {
		platform = os.Getenv("OCI_TO_WSL_PLATFORM")
	}

	if err := registry.PullToTar(profile.Image, tarFile, registry.PullOptions{
		Platform: platform,
	}); err != nil {
		return fmt.Errorf("pulling image: %w", err)
	}
	_ = tarFile.Close()

	slog.Debug("rootfs tar staged", "path", tarPath, "save_tar_mode", saveTar != "")

	// Apply any profile-driven deletions before staging files, so a
	// profile can drop an upstream directory and then place its own
	// replacement at the same destination. File entries with
	// `replace: true` (the default) implicitly contribute their Dst to
	// this delete list, so a staged file fully replaces whatever the
	// upstream image had at the same path rather than overlaying onto it.
	//
	// These tar modifications run in both WSL-import and --save-tar
	// modes. Set OCI_TO_WSL_NO_TAR_MODS=1 to skip them and obtain the
	// rootfs tar exactly as exported from the image (most useful with
	// --save-tar when you want an unmodified artifact).
	deletes := append([]string(nil), profile.Deletes...)
	for _, f := range profile.Files {
		if f.Dst != "" && f.ReplaceEnabled() {
			deletes = append(deletes, f.Dst)
		}
	}
	skipTarMods := isTarModsDisabled()
	if skipTarMods && (len(deletes) > 0 || len(profile.Files) > 0 || len(profile.Users) > 0) {
		// Print directly to stderr so this notice is not suppressed
		// by --loglevel error; it reflects a user-requested
		// behavioural change that should always be visible.
		fmt.Fprintf(os.Stderr, "OCI_TO_WSL_NO_TAR_MODS is set; skipping profile 'deletes' (%d), 'users' (%d) and 'files' (%d) tar modifications\n",
			len(deletes), len(profile.Users), len(profile.Files))
	}
	if !skipTarMods && len(deletes) > 0 {
		slog.Debug("applying profile deletes", "count", len(deletes))
		if err := wsl.ApplyDeletes(tarPath, deletes); err != nil {
			return fmt.Errorf("applying deletes to rootfs tar: %w", err)
		}
	}

	// Create any profile-defined Linux users by editing /etc/passwd,
	// /etc/shadow, and /etc/group inside the rootfs tar. Runs after
	// deletes (so a profile can drop upstream account files and start
	// from a clean slate) and before copies (so a copy targeting the
	// new home directory overlays onto the dir created here).
	if !skipTarMods && len(profile.Users) > 0 {
		usrs := make([]wsl.UserEntry, 0, len(profile.Users))
		for _, u := range profile.Users {
			if strings.TrimSpace(u.Name) == "" {
				return fmt.Errorf("profile users: 'name' is required")
			}
			usrs = append(usrs, wsl.UserEntry{
				Name:          u.Name,
				UID:           u.UID,
				GID:           u.GID,
				Home:          u.Home,
				Shell:         u.Shell,
				Gecos:         u.Gecos,
				Groups:        u.Groups,
				PasswordHash:  u.PasswordHash,
				PasswordPlain: u.PasswordPlain,
				NoCreateHome:  u.NoCreateHome,
			})
		}
		if err := wsl.ApplyUsers(tarPath, usrs); err != nil {
			return fmt.Errorf("creating users in rootfs tar: %w", err)
		}
	}

	// Inject any host files/directories directly into the rootfs tar so
	// they are present in the distribution as soon as wsl.exe --import
	// finishes, before any init_cmds run. This avoids any dependency on a
	// tar binary inside the container.
	if !skipTarMods && len(profile.Files) > 0 {
		slog.Debug("staging profile files into rootfs tar", "count", len(profile.Files))
		injects := make([]wsl.CopyEntry, 0, len(profile.Files))
		for _, f := range profile.Files {
			ce, err := fileEntryToCopy(f)
			if err != nil {
				return err
			}
			injects = append(injects, ce)
		}
		if err := wsl.InjectCopies(tarPath, injects); err != nil {
			return fmt.Errorf("staging files into rootfs tar: %w", err)
		}
	}

	// Apply the wsl_conf profile sugar last so that it can override any
	// /etc/wsl.conf staged via copies (or shipped in the upstream image).
	if profile.WslConf != nil && strings.TrimSpace(profile.WslConf.Content) != "" {
		if err := wsl.ApplyWslConf(tarPath, profile.WslConf.Content, wsl.WslConfMode(profile.WslConf.Mode)); err != nil {
			return fmt.Errorf("applying wsl_conf to rootfs tar: %w", err)
		}
	}

	if saveTar != "" {
		fi, _ := os.Stat(tarPath)
		var size int64
		if fi != nil {
			size = fi.Size()
		}
		slog.Info("wrote rootfs tar", "path", tarPath, "bytes", size)
		fmt.Printf("Wrote rootfs tar to %s", tarPath)
		if fi != nil {
			fmt.Printf(" (%d bytes)", fi.Size())
		}
		fmt.Println()
		return nil
	}

	// Import the rootfs into WSL.
	if err := wsl.Import(wsl.ImportOptions{
		Name:       profile.Name,
		InstallDir: profile.InstallDir,
		RootfsTar:  tarPath,
	}); err != nil {
		return err
	}

	slog.Info("wsl distribution created", "name", profile.Name)
	fmt.Printf("WSL distribution %q created successfully.\n", profile.Name)

	// Run any post-creation initialisation commands.
	for _, c := range profile.InitCmds {
		if err := wsl.RunCommand(profile.Name, c); err != nil {
			return fmt.Errorf("init command %q failed: %w", c, err)
		}
	}

	if len(profile.InitCmds) > 0 {
		slog.Info("initialisation complete", "name", profile.Name, "init_cmds", len(profile.InitCmds))
		fmt.Printf("Initialisation of %q complete.\n", profile.Name)
	}
	return nil
}

// fileEntryToCopy translates a profile FileEntry into the wsl.CopyEntry
// form consumed by InjectCopies. The caller is expected to have run
// Profile.Validate() (or FileEntry.Validate()) first, so well-formedness
// is assumed and any base64 payload has already been decoded and cached
// on the entry.
func fileEntryToCopy(f config.FileEntry) (wsl.CopyEntry, error) {
	switch {
	case f.Src != "":
		return wsl.CopyEntry{Src: f.Src, Dst: f.Dst, Mode: f.Mode}, nil
	case f.Content != nil:
		return wsl.CopyEntry{Data: []byte(*f.Content), Dst: f.Dst, Mode: f.Mode}, nil
	case f.ContentBase64 != nil:
		return wsl.CopyEntry{Data: f.DecodedContentBase64(), Dst: f.Dst, Mode: f.Mode}, nil
	default:
		return wsl.CopyEntry{}, fmt.Errorf("profile files: %q: no source set (call Profile.Validate first)", f.Dst)
	}
}

// envDisableTarMods, when set to a value parseable as true by strconv.ParseBool
// (e.g. "1", "t", "true", "True", "TRUE"), skips the profile-driven tar
// modifications (`deletes` and `files`) so the rootfs tar is left exactly
// as exported from the image. Most useful in --save-tar mode when an
// unmodified artifact is desired.
const envDisableTarMods = "OCI_TO_WSL_NO_TAR_MODS"

func isTarModsDisabled() bool {
	v, err := strconv.ParseBool(os.Getenv(envDisableTarMods))
	if err != nil {
		return false
	}
	return v
}

// dockerLoginCommand defines the `dockerlogin` subcommand. It mirrors a
// subset of `docker login` (server arg, --username, --password,
// --password-stdin) and writes a classic basic-auth entry into
// ~/.docker/config.json so the pull flow can authenticate to a registry
// without docker (or any credential helper) being installed.
func dockerLoginCommand() *cli.Command {
	return &cli.Command{
		Name:      "dockerlogin",
		Usage:     "log in to a registry by writing ~/.docker/config.json (no docker CLI required)",
		ArgsUsage: "[server]",
		Description: `Generate or update a classic docker config.json entry containing
base64-encoded "username:password" credentials, exactly as 'docker login'
would, but without requiring the docker CLI binary to be installed.

The resulting credentials are picked up automatically by the default pull
flow (via go-containerregistry's keychain). If no server argument is given,
Docker Hub is assumed.

Examples:
  # Interactive login to Docker Hub
  oci-to-wsl dockerlogin

  # GHCR with an explicit token, no prompting
  oci-to-wsl dockerlogin ghcr.io --username alice --password-stdin < token.txt`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "username",
				Aliases: []string{"u"},
				Usage:   "registry username",
			},
			&cli.StringFlag{
				Name:    "password",
				Aliases: []string{"p"},
				Usage:   "registry password (prefer --password-stdin to avoid leaking via process listings)",
			},
			&cli.BoolFlag{
				Name:  "password-stdin",
				Usage: "read the password from stdin",
			},
			&cli.StringFlag{
				Name:  "config",
				Usage: "path to the docker config.json to read/update (defaults to $DOCKER_CONFIG/config.json or ~/.docker/config.json)",
			},
			&cli.StringFlag{
				Name:    "output",
				Aliases: []string{"o"},
				Usage:   "write the updated config.json here instead of overwriting --config; use '-' for stdout",
			},
		},
		Action: dockerLoginAction,
	}
}

func dockerLoginAction(_ context.Context, cmd *cli.Command) error {
	server := ""
	if cmd.NArg() > 0 {
		server = cmd.Args().First()
	}

	username := cmd.String("username")
	password := cmd.String("password")
	passwordStdin := cmd.Bool("password-stdin")

	if passwordStdin && password != "" {
		return fmt.Errorf("--password and --password-stdin are mutually exclusive")
	}

	in := bufio.NewReader(os.Stdin)
	if username == "" {
		u, err := promptLine(in, fmt.Sprintf("Username for %s: ", displayServer(server)))
		if err != nil {
			return fmt.Errorf("reading username: %w", err)
		}
		username = strings.TrimSpace(u)
		if username == "" {
			return fmt.Errorf("username is required")
		}
	}

	if passwordStdin {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("reading password from stdin: %w", err)
		}
		// Trim a single trailing newline (the conventional `echo $TOKEN |`
		// shape adds one) but preserve any internal whitespace.
		password = strings.TrimRight(string(data), "\r\n")
	} else if password == "" {
		p, err := promptPassword(fmt.Sprintf("Password for %s: ", displayServer(server)))
		if err != nil {
			return fmt.Errorf("reading password: %w", err)
		}
		password = p
	}

	if password == "" {
		return fmt.Errorf("password is required")
	}

	opts := registry.DockerLoginOptions{
		ConfigPath: cmd.String("config"),
		Server:     server,
		Username:   username,
		Password:   password,
	}

	output := cmd.String("output")
	if output == "-" {
		if err := registry.DockerLoginToWriter(opts, os.Stdout); err != nil {
			return err
		}
		return nil
	}
	if output != "" {
		// --output overrides the on-disk write target while still reading
		// any existing entries from --config / $DOCKER_CONFIG.
		f, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return fmt.Errorf("opening %q: %w", output, err)
		}
		defer func() { _ = f.Close() }()
		if err := registry.DockerLoginToWriter(opts, f); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Login credentials for %s written to %s\n", displayServer(server), output)
		return nil
	}

	path, err := registry.DockerLogin(opts)
	if err != nil {
		return err
	}

	fmt.Printf("Login credentials for %s written to %s\n", displayServer(server), path)
	return nil
}

// displayServer returns the human-readable server name used in prompts and
// log output. An empty server is shown as the docker hub canonical key, to
// match what `docker login` displays.
func displayServer(server string) string {
	if strings.TrimSpace(server) == "" {
		return registry.DefaultDockerHubServer
	}
	return server
}

// promptLine writes prompt to stderr (so it does not pollute stdout) and
// reads a single line from r.
func promptLine(r *bufio.Reader, prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	line, err := r.ReadString('\n')
	if err != nil && (line == "" || err != io.EOF) {
		return "", err
	}
	return line, nil
}

// promptPassword reads a password from the controlling terminal without
// echoing it. When stdin is not a terminal (e.g. piped input in tests), it
// falls back to a plain line read so the command remains scriptable even
// without --password-stdin.
func promptPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		data, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && (line == "" || err != io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
