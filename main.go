package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/urfave/cli/v3"

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

  # Save the rootfs tar for a non-host platform (save-tar mode only)
  OCI_TO_WSL_PLATFORM=linux/arm64 oci-to-wsl --image ubuntu:22.04 --save-tar ubuntu-arm64.tar`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "profile",
				Usage: "path to a YAML profile file (overrides other flags when set)",
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
				Usage: "write the exported rootfs tar to this path and skip 'wsl --import' (useful on non-Windows hosts). Set OCI_TO_WSL_PLATFORM=os/arch (e.g. linux/arm64) to override the image platform; this is only honored in save-tar mode.",
			},
		},
		Action: action,
	}

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
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
	if profile.Image == "" {
		return fmt.Errorf("profile: 'image' is required")
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

	if saveTar != "" {
		fi, _ := os.Stat(tarPath)
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

	fmt.Printf("WSL distribution %q created successfully.\n", profile.Name)

	// Run any post-creation initialisation commands.
	for _, c := range profile.InitCmds {
		if err := wsl.RunCommand(profile.Name, c); err != nil {
			return fmt.Errorf("init command %q failed: %w", c, err)
		}
	}

	if len(profile.InitCmds) > 0 {
		fmt.Printf("Initialisation of %q complete.\n", profile.Name)
	}
	return nil
}
