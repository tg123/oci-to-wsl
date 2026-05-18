package wsl

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// UserEntry describes a Linux user to create inside the imported WSL
// distribution by editing /etc/passwd, /etc/shadow, and /etc/group
// directly in the rootfs tar. This avoids any dependency on useradd /
// adduser inside the container and means the user exists on first boot —
// before any init_cmds run.
type UserEntry struct {
	// Name is the login name. Required. Creation fails if an entry with
	// the same name already exists in /etc/passwd.
	Name string

	// UID, when > 0, sets the numeric user id. When 0, a free id is
	// allocated automatically starting at 1000. Reusing an id already
	// present in /etc/passwd is an error.
	UID int

	// GID, when > 0, sets the primary group id. When 0, GID defaults to
	// the resolved UID when that id is free, otherwise to the next free
	// id starting at 1000. A matching entry in /etc/group is created on
	// demand if none with that gid already exists.
	GID int

	// Home is the absolute POSIX path of the user's home directory.
	// Defaults to "/home/<Name>".
	Home string

	// Shell is the user's login shell. Defaults to "/bin/sh".
	Shell string

	// Gecos is the comment / full-name field in /etc/passwd. Optional.
	Gecos string

	// Groups is a list of supplementary group names to add the user to.
	// Groups that don't exist in /etc/group are silently skipped so a
	// profile can portably ask for e.g. "sudo" or "wheel" without
	// breaking on images that lack them.
	Groups []string

	// PasswordHash is written verbatim into the password field of the
	// /etc/shadow entry. An empty value disables password login by
	// writing "!". To set a real password, supply a hash produced by
	// e.g. `openssl passwd -6`.
	PasswordHash string

	// NoCreateHome, when true, suppresses creation of the home directory
	// entry in the rootfs tar. The default (false) emits a directory
	// entry at Home owned by the new uid:gid with mode 0700.
	NoCreateHome bool
}

const (
	tarEtcPasswd = "etc/passwd"
	tarEtcShadow = "etc/shadow"
	tarEtcGroup  = "etc/group"
)

// ApplyUsers creates the requested users by rewriting /etc/passwd,
// /etc/shadow, and /etc/group inside the rootfs tar at tarPath, and
// optionally appending a home directory entry for each user. It is a
// no-op when users is empty.
//
// The function walks the tar twice: a first pass to load the existing
// account files (typically a few KB each), and a second streaming pass
// that substitutes the new bodies and appends the home directories. Any
// of the three account files that aren't present in the source tar are
// created with sensible default modes.
func ApplyUsers(tarPath string, users []UserEntry) error {
	if len(users) == 0 {
		return nil
	}
	// Normalize once and validate. The normalized values flow into
	// mergeUsers so what we dedup-check matches what we actually write.
	seen := make(map[string]struct{}, len(users))
	for i := range users {
		u := &users[i]
		name := strings.TrimSpace(u.Name)
		if name == "" {
			return fmt.Errorf("user entry %d: 'name' is required", i)
		}
		if err := validateAccountField("name", name); err != nil {
			return fmt.Errorf("user entry %d: %w", i, err)
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("user %q listed more than once", name)
		}
		seen[name] = struct{}{}
		u.Name = name
		if u.Home != "" {
			if err := validateAccountField("home", u.Home); err != nil {
				return fmt.Errorf("user %q: %w", name, err)
			}
		}
		if u.Shell != "" {
			if err := validateAccountField("shell", u.Shell); err != nil {
				return fmt.Errorf("user %q: %w", name, err)
			}
		}
		if u.Gecos != "" {
			if err := validateAccountField("gecos", u.Gecos); err != nil {
				return fmt.Errorf("user %q: %w", name, err)
			}
		}
		for _, g := range u.Groups {
			if err := validateAccountField("group", g); err != nil {
				return fmt.Errorf("user %q: %w", name, err)
			}
		}
		if u.PasswordHash != "" {
			// The hash is written into /etc/shadow as-is; newline/NUL
			// would terminate the record early and let arbitrary lines
			// follow. ':' is allowed only via aging-fields suffixes, so
			// reject any ':' too — supply a bare crypt hash.
			if err := validateAccountField("password_hash", u.PasswordHash); err != nil {
				return fmt.Errorf("user %q: %w", name, err)
			}
		}
	}

	passwd, shadow, group, err := readAccountFiles(tarPath)
	if err != nil {
		return err
	}

	resolved, newPasswd, newShadow, newGroup, err := mergeUsers(passwd, shadow, group, users)
	if err != nil {
		return err
	}

	return rewriteTarWithAccounts(tarPath, newPasswd, newShadow, newGroup, resolved)
}

// accountFile is the in-memory representation of /etc/passwd, /etc/shadow
// or /etc/group as read from (or about to be written to) the tar. present
// is false when the source tar didn't contain the file at all; in that
// case header is filled in with a synthetic default when we later need
// to add one.
type accountFile struct {
	present bool
	header  tar.Header
	body    []byte
}

func readAccountFiles(tarPath string) (passwd, shadow, group accountFile, err error) {
	f, oerr := os.Open(tarPath) //nolint:gosec
	if oerr != nil {
		err = fmt.Errorf("open rootfs tar %q: %w", tarPath, oerr)
		return
	}
	defer func() { _ = f.Close() }()

	tr := tar.NewReader(f)
	for {
		hdr, herr := tr.Next()
		if herr == io.EOF {
			break
		}
		if herr != nil {
			err = fmt.Errorf("reading tar entry: %w", herr)
			return
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := normalizeTarName(hdr.Name)
		var slot *accountFile
		switch name {
		case tarEtcPasswd:
			slot = &passwd
		case tarEtcShadow:
			slot = &shadow
		case tarEtcGroup:
			slot = &group
		default:
			continue
		}
		buf, rerr := io.ReadAll(tr)
		if rerr != nil {
			err = fmt.Errorf("reading %q: %w", hdr.Name, rerr)
			return
		}
		*slot = accountFile{present: true, header: *hdr, body: buf}
	}
	return
}

// resolvedUser captures the final (uid, gid) for an input user after
// auto-allocation so the second pass can emit a matching home directory.
type resolvedUser struct {
	in  UserEntry
	uid int
	gid int
}

// mergeUsers computes updated /etc/passwd, /etc/shadow, and /etc/group
// bodies from the existing ones plus the requested users. It allocates
// missing uids/gids from the 1000-64999 range and validates that
// explicit ids and names don't collide with existing entries.
func mergeUsers(passwd, shadow, group accountFile, users []UserEntry) (
	resolved []resolvedUser,
	newPasswd, newShadow, newGroup accountFile,
	err error,
) {
	passwdLines := splitLines(passwd.body)
	shadowLines := splitLines(shadow.body)
	groupLines := splitLines(group.body)

	existingPasswd := map[string]int{}
	usedUIDs := map[int]struct{}{}
	for _, l := range passwdLines {
		fields := strings.SplitN(l, ":", 7)
		if len(fields) < 3 {
			continue
		}
		if uid, perr := strconv.Atoi(fields[2]); perr == nil {
			usedUIDs[uid] = struct{}{}
			existingPasswd[fields[0]] = uid
		}
	}
	shadowHas := map[string]struct{}{}
	for _, l := range shadowLines {
		fields := strings.SplitN(l, ":", 2)
		if len(fields) < 1 {
			continue
		}
		shadowHas[fields[0]] = struct{}{}
	}
	existingGroups := map[string]int{}
	usedGIDs := map[int]struct{}{}
	groupLineIdx := map[string]int{}
	for i, l := range groupLines {
		fields := strings.SplitN(l, ":", 4)
		if len(fields) < 3 {
			continue
		}
		if gid, perr := strconv.Atoi(fields[2]); perr == nil {
			usedGIDs[gid] = struct{}{}
			existingGroups[fields[0]] = gid
		}
		groupLineIdx[fields[0]] = i
	}

	nextFreeID := func(used map[int]struct{}, prefer int) int {
		if prefer > 0 {
			if _, taken := used[prefer]; !taken {
				return prefer
			}
		}
		for v := 1000; v < 65000; v++ {
			if _, taken := used[v]; !taken {
				return v
			}
		}
		return -1
	}

	resolved = make([]resolvedUser, 0, len(users))
	for _, u := range users {
		name := u.Name
		if _, ok := existingPasswd[name]; ok {
			err = fmt.Errorf("user %q already exists in /etc/passwd", name)
			return
		}

		uid := u.UID
		if uid <= 0 {
			uid = nextFreeID(usedUIDs, 0)
			if uid < 0 {
				err = fmt.Errorf("user %q: no free uid available in the 1000-64999 range", name)
				return
			}
		} else if _, taken := usedUIDs[uid]; taken {
			err = fmt.Errorf("user %q: uid %d is already in use", name, uid)
			return
		}
		usedUIDs[uid] = struct{}{}

		gid := u.GID
		if gid <= 0 {
			gid = nextFreeID(usedGIDs, uid)
			if gid < 0 {
				err = fmt.Errorf("user %q: no free gid available in the 1000-64999 range", name)
				return
			}
		}

		// Ensure a group entry exists for the primary gid. If no group
		// uses this gid yet, create one named after the user.
		gidHasEntry := false
		for _, g := range existingGroups {
			if g == gid {
				gidHasEntry = true
				break
			}
		}
		if !gidHasEntry {
			if _, nameTaken := existingGroups[name]; nameTaken {
				err = fmt.Errorf("user %q: cannot create primary group (name already exists with a different gid)", name)
				return
			}
			groupLines = append(groupLines, fmt.Sprintf("%s:x:%d:", name, gid))
			existingGroups[name] = gid
			usedGIDs[gid] = struct{}{}
			groupLineIdx[name] = len(groupLines) - 1
		}

		home := u.Home
		if home == "" {
			home = "/home/" + name
		}
		shell := u.Shell
		if shell == "" {
			shell = "/bin/sh"
		}
		passwdLines = append(passwdLines, fmt.Sprintf("%s:x:%d:%d:%s:%s:%s", name, uid, gid, u.Gecos, home, shell))
		existingPasswd[name] = uid

		hash := u.PasswordHash
		if hash == "" {
			hash = "!"
		}
		if _, dup := shadowHas[name]; !dup {
			// /etc/shadow: name:hash:lastchg:min:max:warn:inactive:expire:reserved
			shadowLines = append(shadowLines, fmt.Sprintf("%s:%s:::::::", name, hash))
			shadowHas[name] = struct{}{}
		}

		// Append the new user as a member of every supplementary group
		// that exists. Sort for deterministic output.
		extras := append([]string(nil), u.Groups...)
		sort.Strings(extras)
		for _, g := range extras {
			idx, ok := groupLineIdx[g]
			if !ok {
				continue // missing group: silent skip
			}
			parts := strings.SplitN(groupLines[idx], ":", 4)
			for len(parts) < 4 {
				parts = append(parts, "")
			}
			members := []string{}
			if parts[3] != "" {
				members = strings.Split(parts[3], ",")
			}
			alreadyIn := false
			for _, m := range members {
				if m == name {
					alreadyIn = true
					break
				}
			}
			if !alreadyIn {
				members = append(members, name)
			}
			parts[3] = strings.Join(members, ",")
			groupLines[idx] = strings.Join(parts, ":")
		}

		resolved = append(resolved, resolvedUser{in: u, uid: uid, gid: gid})
	}

	newPasswd = bodyWith(passwd, joinLines(passwdLines), tarEtcPasswd, 0o644)
	newShadow = bodyWith(shadow, joinLines(shadowLines), tarEtcShadow, 0o640)
	newGroup = bodyWith(group, joinLines(groupLines), tarEtcGroup, 0o644)
	return
}

// validateAccountField rejects characters that would corrupt the
// colon-delimited account files. A literal ':' or '\n' would split a
// record and could be used to inject extra entries; '\r' likewise breaks
// parsers that treat it as a line terminator. Null bytes are never
// valid in these files.
func validateAccountField(kind, v string) error {
	if strings.ContainsAny(v, ":\n\r\x00") {
		return fmt.Errorf("%s %q contains an invalid character (':' '\\n' '\\r' or NUL)", kind, v)
	}
	return nil
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := strings.TrimRight(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func joinLines(lines []string) []byte {
	if len(lines) == 0 {
		return nil
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// bodyWith returns a copy of orig with body replaced and Size updated.
// When orig was absent from the source tar, a synthetic header with
// defaultMode is materialised so the file can be appended later.
func bodyWith(orig accountFile, body []byte, name string, defaultMode int64) accountFile {
	out := orig
	out.body = body
	if !orig.present {
		out.present = true
		out.header = tar.Header{
			Name:     name,
			Mode:     defaultMode,
			Typeflag: tar.TypeReg,
			Format:   tar.FormatPAX,
		}
	}
	out.header.Size = int64(len(body))
	return out
}

// rewriteTarWithAccounts streams the source tar to a sibling file,
// substituting the three account files with the updated bodies and
// appending any missing account files plus per-user home directories.
// On success the new file atomically replaces the original.
func rewriteTarWithAccounts(
	tarPath string,
	newPasswd, newShadow, newGroup accountFile,
	resolved []resolvedUser,
) error {
	in, err := os.Open(tarPath) //nolint:gosec
	if err != nil {
		return fmt.Errorf("open rootfs tar %q: %w", tarPath, err)
	}
	// Close in via defer on any error path; on the success path we close
	// it explicitly before os.Rename (needed on Windows) and set the
	// local to nil so this defer becomes a no-op rather than a double
	// Close.
	defer func() {
		if in != nil {
			_ = in.Close()
		}
	}()

	outPath := tarPath + ".users"
	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec
	if err != nil {
		return fmt.Errorf("create %q: %w", outPath, err)
	}
	cleanup := func() { _ = os.Remove(outPath) }

	tr := tar.NewReader(in)
	tw := tar.NewWriter(out)

	abort := func(format string, args ...any) error {
		_ = tw.Close()
		_ = out.Close()
		cleanup()
		return fmt.Errorf(format, args...)
	}

	// addedDirs tracks every directory that already exists in (or has
	// been written to) the output tar, so writeParentDirs can skip them
	// when emitting synthesized account files or home directory entries.
	// We also seed it from the parent path of every regular file we see,
	// which mirrors the implicit directory tree contained in the source
	// tar even when the upstream image omits explicit dir entries.
	addedDirs := map[string]struct{}{}
	noteDir := func(p string) {
		p = strings.TrimSuffix(p, "/")
		if p == "" || p == "." {
			return
		}
		addedDirs[p] = struct{}{}
	}
	noteParents := func(p string) {
		p = strings.TrimSuffix(p, "/")
		for {
			i := strings.LastIndex(p, "/")
			if i <= 0 {
				return
			}
			p = p[:i]
			noteDir(p)
		}
	}

	wrotePasswd, wroteShadow, wroteGroup := false, false, false
	for {
		hdr, rerr := tr.Next()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return abort("reading tar entry: %w", rerr)
		}
		name := normalizeTarName(hdr.Name)
		if hdr.Typeflag == tar.TypeDir {
			noteDir(name)
		} else {
			noteParents(name)
		}
		var sub *accountFile
		if hdr.Typeflag == tar.TypeReg {
			switch name {
			case tarEtcPasswd:
				sub = &newPasswd
				wrotePasswd = true
			case tarEtcShadow:
				sub = &newShadow
				wroteShadow = true
			case tarEtcGroup:
				sub = &newGroup
				wroteGroup = true
			}
		}
		if sub != nil {
			// Keep the original header metadata (mode, uid/gid, mtime),
			// just update Size, then drop the original body and write
			// the new one.
			h := *hdr
			h.Size = int64(len(sub.body))
			if err := tw.WriteHeader(&h); err != nil {
				return abort("writing tar header %q: %w", hdr.Name, err)
			}
			if hdr.Size > 0 {
				if _, err := io.Copy(io.Discard, tr); err != nil {
					return abort("discarding original %q: %w", hdr.Name, err)
				}
			}
			if len(sub.body) > 0 {
				if _, err := tw.Write(sub.body); err != nil {
					return abort("writing %q body: %w", hdr.Name, err)
				}
			}
			continue
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return abort("writing tar header %q: %w", hdr.Name, err)
		}
		if hdr.Size > 0 {
			if _, err := io.Copy(tw, tr); err != nil {
				return abort("copying tar body %q: %w", hdr.Name, err)
			}
		}
	}

	// Append any account files that didn't exist in the source tar so
	// the new user is still functional on minimal images.
	for _, af := range []struct {
		wrote bool
		file  accountFile
	}{
		{wrotePasswd, newPasswd},
		{wroteShadow, newShadow},
		{wroteGroup, newGroup},
	} {
		if af.wrote || !af.file.present {
			continue
		}
		if err := writeParentDirs(tw, af.file.header.Name, addedDirs); err != nil {
			return abort("writing parent dirs for %q: %w", af.file.header.Name, err)
		}
		h := af.file.header
		h.Size = int64(len(af.file.body))
		if err := tw.WriteHeader(&h); err != nil {
			return abort("writing tar header %q: %w", h.Name, err)
		}
		if len(af.file.body) > 0 {
			if _, err := tw.Write(af.file.body); err != nil {
				return abort("writing %q body: %w", h.Name, err)
			}
		}
	}

	// Append home directory entries. Dedup across users so two users
	// sharing a home (unusual but possible) don't produce duplicate
	// entries.
	addedHomes := map[string]struct{}{}
	for _, ru := range resolved {
		if ru.in.NoCreateHome {
			continue
		}
		home := ru.in.Home
		if home == "" {
			home = "/home/" + ru.in.Name
		}
		tarName, terr := toTarPath(home)
		if terr != nil {
			return abort("user %q: invalid home %q: %w", ru.in.Name, home, terr)
		}
		if _, ok := addedHomes[tarName]; ok {
			continue
		}
		addedHomes[tarName] = struct{}{}
		if err := writeParentDirs(tw, tarName, addedDirs); err != nil {
			return abort("writing parent dirs for home %q: %w", home, err)
		}
		if _, dup := addedDirs[tarName]; dup {
			// A directory with this name was already emitted (e.g., the
			// source tar contained it explicitly). Don't write a second
			// header for the same path — tar consumers handle this
			// inconsistently, and we'd otherwise produce a duplicate
			// entry with different ownership.
			continue
		}
		hdr := &tar.Header{
			Name:     tarName + "/",
			Mode:     0o700,
			Uid:      ru.uid,
			Gid:      ru.gid,
			Typeflag: tar.TypeDir,
			Format:   tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return abort("writing home dir %q: %w", home, err)
		}
		noteDir(tarName)
	}

	if err := tw.Close(); err != nil {
		_ = out.Close()
		cleanup()
		return fmt.Errorf("closing rewritten tar: %w", err)
	}
	if err := out.Close(); err != nil {
		cleanup()
		return fmt.Errorf("closing rewritten tar file: %w", err)
	}
	if err := in.Close(); err != nil {
		cleanup()
		return fmt.Errorf("closing source tar: %w", err)
	}
	in = nil
	if err := os.Rename(outPath, tarPath); err != nil {
		cleanup()
		return fmt.Errorf("replacing rootfs tar: %w", err)
	}
	return nil
}
