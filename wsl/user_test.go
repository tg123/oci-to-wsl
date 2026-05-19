package wsl_test

import (
	"archive/tar"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/tg123/oci-to-wsl/wsl"
)

// baseAccountTar writes a minimal /etc/passwd, /etc/shadow, /etc/group
// triple plus an unrelated file so we can assert untouched entries
// survive a rewrite. Returned path holds the tar archive.
func baseAccountTar(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, []tarEntry{
		{hdr: tar.Header{Name: "etc/passwd", Mode: 0o644, Typeflag: tar.TypeReg},
			body: []byte("root:x:0:0:root:/root:/bin/sh\nbin:x:1:1:bin:/bin:/sbin/nologin\n")},
		{hdr: tar.Header{Name: "etc/shadow", Mode: 0o640, Typeflag: tar.TypeReg},
			body: []byte("root:*:::::::\nbin:*:::::::\n")},
		{hdr: tar.Header{Name: "etc/group", Mode: 0o644, Typeflag: tar.TypeReg},
			body: []byte("root:x:0:\nbin:x:1:\nsudo:x:27:\n")},
		{hdr: tar.Header{Name: "etc/hostname", Mode: 0o644, Typeflag: tar.TypeReg},
			body: []byte("alpine\n")},
	})
	return tarPath
}

// findPasswdLine returns the colon-split fields for the named user in
// the /etc/passwd body, or nil when absent.
func findPasswdLine(body []byte, name string) []string {
	for _, l := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		f := strings.SplitN(l, ":", 7)
		if len(f) >= 1 && f[0] == name {
			return f
		}
	}
	return nil
}

// findGroupLine returns the colon-split fields for the named group in
// the /etc/group body, or nil when absent.
func findGroupLine(body []byte, name string) []string {
	for _, l := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		f := strings.SplitN(l, ":", 4)
		if len(f) >= 1 && f[0] == name {
			return f
		}
	}
	return nil
}

func TestApplyUsers_AutoAllocateUIDAndGID(t *testing.T) {
	tarPath := baseAccountTar(t)
	if err := wsl.ApplyUsers(tarPath, []wsl.UserEntry{{Name: "alice"}}); err != nil {
		t.Fatalf("ApplyUsers: %v", err)
	}
	got := readTar(t, tarPath)

	pw := findPasswdLine(got["etc/passwd"].body, "alice")
	if pw == nil {
		t.Fatalf("alice not in /etc/passwd: %q", got["etc/passwd"].body)
	}
	if pw[2] != "1000" || pw[3] != "1000" {
		t.Fatalf("expected uid=1000 gid=1000, got uid=%s gid=%s", pw[2], pw[3])
	}
	if pw[5] != "/home/alice" {
		t.Fatalf("expected home=/home/alice, got %q", pw[5])
	}
	if pw[6] != "/bin/sh" {
		t.Fatalf("expected shell=/bin/sh, got %q", pw[6])
	}

	sh := findPasswdLine(got["etc/shadow"].body, "alice") // same parser works
	if sh == nil {
		t.Fatalf("alice not in /etc/shadow: %q", got["etc/shadow"].body)
	}
	if sh[1] != "!" {
		t.Fatalf("expected shadow hash=!, got %q", sh[1])
	}

	// A matching primary group must have been created.
	gr := findGroupLine(got["etc/group"].body, "alice")
	if gr == nil {
		t.Fatalf("primary group alice not created: %q", got["etc/group"].body)
	}
	if gr[2] != "1000" {
		t.Fatalf("expected primary gid=1000, got %s", gr[2])
	}

	// Home directory entry present with ownership.
	home, ok := got["home/alice/"]
	if !ok {
		t.Fatalf("home/alice/ not present; entries: %v", keys(got))
	}
	if home.hdr.Typeflag != tar.TypeDir {
		t.Fatalf("home is not a dir: %v", home.hdr.Typeflag)
	}
	if home.hdr.Uid != 1000 || home.hdr.Gid != 1000 {
		t.Fatalf("expected home owned by 1000:1000, got %d:%d", home.hdr.Uid, home.hdr.Gid)
	}
	if home.hdr.Mode != 0o700 {
		t.Fatalf("expected home mode 0700, got %o", home.hdr.Mode)
	}

	// Unrelated entries preserved.
	if string(got["etc/hostname"].body) != "alpine\n" {
		t.Fatalf("etc/hostname body changed: %q", got["etc/hostname"].body)
	}
}

func TestApplyUsers_ExplicitFieldsAndExtraGroups(t *testing.T) {
	tarPath := baseAccountTar(t)
	err := wsl.ApplyUsers(tarPath, []wsl.UserEntry{{
		Name:         "bob",
		UID:          1500,
		GID:          1500,
		Home:         "/srv/bob",
		Shell:        "/bin/bash",
		Gecos:        "Bob Builder",
		Groups:       []string{"sudo", "doesnotexist"},
		PasswordHash: "$6$abc$hashvalue",
	}})
	if err != nil {
		t.Fatalf("ApplyUsers: %v", err)
	}
	got := readTar(t, tarPath)

	pw := findPasswdLine(got["etc/passwd"].body, "bob")
	want := []string{"bob", "x", "1500", "1500", "Bob Builder", "/srv/bob", "/bin/bash"}
	if len(pw) != len(want) {
		t.Fatalf("passwd field count: got %d want %d (%q)", len(pw), len(want), pw)
	}
	for i := range want {
		if pw[i] != want[i] {
			t.Fatalf("passwd field %d: got %q want %q", i, pw[i], want[i])
		}
	}

	sh := findPasswdLine(got["etc/shadow"].body, "bob")
	if sh[1] != "$6$abc$hashvalue" {
		t.Fatalf("shadow hash: got %q", sh[1])
	}

	// sudo group should include bob; doesnotexist must NOT be created.
	sudo := findGroupLine(got["etc/group"].body, "sudo")
	if len(sudo) < 4 {
		t.Fatalf("sudo group malformed: %v", sudo)
	}
	if !contains(strings.Split(sudo[3], ","), "bob") {
		t.Fatalf("bob not added to sudo group: %q", sudo[3])
	}
	if findGroupLine(got["etc/group"].body, "doesnotexist") != nil {
		t.Fatal("missing group should not have been created")
	}

	// Home should be /srv/bob.
	if _, ok := got["srv/bob/"]; !ok {
		t.Fatalf("srv/bob/ home dir not present; entries: %v", keys(got))
	}
}

func TestApplyUsers_NoCreateHome(t *testing.T) {
	tarPath := baseAccountTar(t)
	if err := wsl.ApplyUsers(tarPath, []wsl.UserEntry{{Name: "carol", NoCreateHome: true}}); err != nil {
		t.Fatalf("ApplyUsers: %v", err)
	}
	got := readTar(t, tarPath)
	if _, ok := got["home/carol/"]; ok {
		t.Fatalf("home/carol/ should not have been created with NoCreateHome=true")
	}
	if findPasswdLine(got["etc/passwd"].body, "carol") == nil {
		t.Fatal("carol should still be in /etc/passwd")
	}
}

func TestApplyUsers_DuplicateUIDRejected(t *testing.T) {
	tarPath := baseAccountTar(t)
	// uid 1 is already used by 'bin' in the seed tar.
	err := wsl.ApplyUsers(tarPath, []wsl.UserEntry{{Name: "dave", UID: 1}})
	if err == nil {
		t.Fatal("expected error when uid collides with existing user")
	}
}

func TestApplyUsers_DuplicateNameRejected(t *testing.T) {
	tarPath := baseAccountTar(t)
	err := wsl.ApplyUsers(tarPath, []wsl.UserEntry{{Name: "root"}})
	if err == nil {
		t.Fatal("expected error when user name already exists in /etc/passwd")
	}
}

func TestApplyUsers_DuplicateNameInInputRejected(t *testing.T) {
	tarPath := baseAccountTar(t)
	err := wsl.ApplyUsers(tarPath, []wsl.UserEntry{{Name: "eve"}, {Name: "eve"}})
	if err == nil {
		t.Fatal("expected error when the same user is listed twice")
	}
}

func TestApplyUsers_EmptyNameRejected(t *testing.T) {
	tarPath := baseAccountTar(t)
	err := wsl.ApplyUsers(tarPath, []wsl.UserEntry{{Name: "  "}})
	if err == nil {
		t.Fatal("expected error for blank user name")
	}
}

func TestApplyUsers_InjectionCharsRejected(t *testing.T) {
	// User-controlled fields are written verbatim into colon-delimited
	// files; ':' or '\n' in any of them would let a profile inject
	// extra /etc/passwd entries. Make sure each is rejected.
	cases := []struct {
		name string
		u    wsl.UserEntry
	}{
		{"colon_in_name", wsl.UserEntry{Name: "ev:il"}},
		{"newline_in_name", wsl.UserEntry{Name: "ev\nil"}},
		{"colon_in_home", wsl.UserEntry{Name: "ok", Home: "/home/x:y"}},
		{"newline_in_shell", wsl.UserEntry{Name: "ok", Shell: "/bin/sh\nroot::0:0::/:/bin/sh"}},
		{"colon_in_gecos", wsl.UserEntry{Name: "ok", Gecos: "a:b"}},
		{"colon_in_group", wsl.UserEntry{Name: "ok", Groups: []string{"sudo:foo"}}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			tarPath := baseAccountTar(t)
			if err := wsl.ApplyUsers(tarPath, []wsl.UserEntry{tc.u}); err == nil {
				t.Fatalf("expected ApplyUsers to reject %+v", tc.u)
			}
		})
	}
}

func TestApplyUsers_InvalidHomePathRejected(t *testing.T) {
	// Home is documented as an absolute POSIX path; relative or
	// escape-out-of-rootfs values would corrupt the /etc/passwd record
	// and (with NoCreateHome=false) the tar layout too. Reject up front
	// regardless of NoCreateHome, since the same value still lands in
	// /etc/passwd either way.
	cases := []struct {
		name string
		u    wsl.UserEntry
	}{
		{"relative_home", wsl.UserEntry{Name: "a", Home: "home/a"}},
		{"dot_home", wsl.UserEntry{Name: "c", Home: "/./"}},
		{"relative_home_no_create", wsl.UserEntry{Name: "d", Home: "tmp", NoCreateHome: true}},
		{"dotdot_prefix_home", wsl.UserEntry{Name: "e", Home: "/..foo", NoCreateHome: true}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			tarPath := baseAccountTar(t)
			if err := wsl.ApplyUsers(tarPath, []wsl.UserEntry{tc.u}); err == nil {
				t.Fatalf("expected ApplyUsers to reject %+v", tc.u)
			}
		})
	}
}

func TestApplyUsers_DuplicateNameDetectedWithNonNumericUID(t *testing.T) {
	// If an existing /etc/passwd line has a non-numeric uid, dedup must
	// still catch a request for the same login name; otherwise a profile
	// could end up with two records for the same user.
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, []tarEntry{
		{hdr: tar.Header{Name: "etc/passwd", Mode: 0o644, Typeflag: tar.TypeReg},
			body: []byte("weird:x:notanumber:0::/:/bin/sh\n")},
		{hdr: tar.Header{Name: "etc/shadow", Mode: 0o640, Typeflag: tar.TypeReg},
			body: []byte("weird:*:::::::\n")},
		{hdr: tar.Header{Name: "etc/group", Mode: 0o644, Typeflag: tar.TypeReg},
			body: []byte("weird:x:bogus:\n")},
	})
	if err := wsl.ApplyUsers(tarPath, []wsl.UserEntry{{Name: "weird"}}); err == nil {
		t.Fatal("expected error: duplicate name should be caught even when existing uid is non-numeric")
	}
}

func TestApplyUsers_ReplacesStaleShadowEntry(t *testing.T) {
	// A passwd entry can be missing while shadow still carries a stale
	// record for the same name (e.g. partial image cleanup). The new
	// hash must replace the stale one so passwd and shadow stay
	// consistent.
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, []tarEntry{
		{hdr: tar.Header{Name: "etc/passwd", Mode: 0o644, Typeflag: tar.TypeReg},
			body: []byte("root:x:0:0:root:/root:/bin/sh\n")},
		{hdr: tar.Header{Name: "etc/shadow", Mode: 0o640, Typeflag: tar.TypeReg},
			body: []byte("root:*:::::::\nstale:$6$old$old:::::::\n")},
		{hdr: tar.Header{Name: "etc/group", Mode: 0o644, Typeflag: tar.TypeReg},
			body: []byte("root:x:0:\n")},
	})
	if err := wsl.ApplyUsers(tarPath, []wsl.UserEntry{{Name: "stale", PasswordHash: "$6$new$new"}}); err != nil {
		t.Fatalf("ApplyUsers: %v", err)
	}
	got := readTar(t, tarPath)
	shadowBody := string(got["etc/shadow"].body)
	if strings.Contains(shadowBody, "$6$old$old") {
		t.Fatalf("stale shadow hash should have been replaced, body=%q", shadowBody)
	}
	if !strings.Contains(shadowBody, "stale:$6$new$new:::::::") {
		t.Fatalf("expected new shadow entry for stale, body=%q", shadowBody)
	}
	// And only one entry for that name should remain.
	count := 0
	for _, l := range strings.Split(strings.TrimRight(shadowBody, "\n"), "\n") {
		f := strings.SplitN(l, ":", 2)
		if len(f) > 0 && f[0] == "stale" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one shadow entry for 'stale', got %d (body=%q)", count, shadowBody)
	}
}

func TestApplyUsers_RemovesDuplicateStaleShadowEntries(t *testing.T) {
	// If /etc/shadow ships multiple stale entries for the same name,
	// ApplyUsers must replace the first (so a parser that returns the
	// first match still sees the new hash) and drop the rest, leaving
	// exactly one entry for the name.
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, []tarEntry{
		{hdr: tar.Header{Name: "etc/passwd", Mode: 0o644, Typeflag: tar.TypeReg},
			body: []byte("root:x:0:0:root:/root:/bin/sh\n")},
		{hdr: tar.Header{Name: "etc/shadow", Mode: 0o640, Typeflag: tar.TypeReg},
			body: []byte("root:*:::::::\ndup:$6$a$a:::::::\nother:*:::::::\ndup:$6$b$b:::::::\n")},
		{hdr: tar.Header{Name: "etc/group", Mode: 0o644, Typeflag: tar.TypeReg},
			body: []byte("root:x:0:\n")},
	})
	if err := wsl.ApplyUsers(tarPath, []wsl.UserEntry{{Name: "dup", PasswordHash: "$6$new$new"}}); err != nil {
		t.Fatalf("ApplyUsers: %v", err)
	}
	got := readTar(t, tarPath)
	shadowBody := string(got["etc/shadow"].body)
	if strings.Contains(shadowBody, "$6$a$a") || strings.Contains(shadowBody, "$6$b$b") {
		t.Fatalf("stale shadow hashes not fully removed, body=%q", shadowBody)
	}
	count := 0
	for _, l := range strings.Split(strings.TrimRight(shadowBody, "\n"), "\n") {
		f := strings.SplitN(l, ":", 2)
		if len(f) > 0 && f[0] == "dup" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one shadow entry for 'dup', got %d (body=%q)", count, shadowBody)
	}
	if !strings.Contains(shadowBody, "dup:$6$new$new:::::::") {
		t.Fatalf("expected new shadow entry for dup, body=%q", shadowBody)
	}
	// And the other unrelated entry must still be present.
	if !strings.Contains(shadowBody, "other:*:::::::") {
		t.Fatalf("unrelated shadow entry was lost, body=%q", shadowBody)
	}
}

func TestApplyUsers_PasswordPlainHashedIntoShadow(t *testing.T) {
	// PasswordPlain should be hashed with SHA-512 crypt ($6$) before
	// hitting /etc/shadow — and never appear there literally.
	tarPath := baseAccountTar(t)
	if err := wsl.ApplyUsers(tarPath, []wsl.UserEntry{{Name: "alice", PasswordPlain: "s3cret!"}}); err != nil {
		t.Fatalf("ApplyUsers: %v", err)
	}
	got := readTar(t, tarPath)
	shadowBody := string(got["etc/shadow"].body)
	if strings.Contains(shadowBody, "s3cret!") {
		t.Fatalf("plaintext leaked into /etc/shadow: %q", shadowBody)
	}
	// Find alice's shadow line and verify it carries a $6$ SHA-512 crypt hash.
	var aliceLine string
	for _, l := range strings.Split(strings.TrimRight(shadowBody, "\n"), "\n") {
		f := strings.SplitN(l, ":", 2)
		if len(f) >= 1 && f[0] == "alice" {
			aliceLine = l
			break
		}
	}
	if aliceLine == "" {
		t.Fatalf("alice not in /etc/shadow: %q", shadowBody)
	}
	parts := strings.Split(aliceLine, ":")
	if len(parts) < 2 || !strings.HasPrefix(parts[1], "$6$") {
		t.Fatalf("expected SHA-512 crypt hash ($6$...), got %q", aliceLine)
	}
}

func TestApplyUsers_PasswordPlainAndHashMutuallyExclusive(t *testing.T) {
	tarPath := baseAccountTar(t)
	err := wsl.ApplyUsers(tarPath, []wsl.UserEntry{{
		Name:          "alice",
		PasswordHash:  "$6$abc$def",
		PasswordPlain: "p",
	}})
	if err == nil {
		t.Fatal("expected error when both password_hash and password_plain are set")
	}
}

func TestApplyUsers_EmptyIsNoop(t *testing.T) {
	tarPath := baseAccountTar(t)
	before := readTar(t, tarPath)
	if err := wsl.ApplyUsers(tarPath, nil); err != nil {
		t.Fatal(err)
	}
	after := readTar(t, tarPath)
	if len(before) != len(after) {
		t.Fatalf("nil users should be a no-op (entry count %d -> %d)", len(before), len(after))
	}
}

func TestApplyUsers_MultipleAutoAllocatesDistinct(t *testing.T) {
	tarPath := baseAccountTar(t)
	err := wsl.ApplyUsers(tarPath, []wsl.UserEntry{{Name: "u1"}, {Name: "u2"}, {Name: "u3"}})
	if err != nil {
		t.Fatalf("ApplyUsers: %v", err)
	}
	got := readTar(t, tarPath)
	uids := []int{}
	for _, n := range []string{"u1", "u2", "u3"} {
		pw := findPasswdLine(got["etc/passwd"].body, n)
		if pw == nil {
			t.Fatalf("%s missing", n)
		}
		uids = append(uids, atoi(t, pw[2]))
	}
	sort.Ints(uids)
	for i := 0; i < len(uids)-1; i++ {
		if uids[i] == uids[i+1] {
			t.Fatalf("duplicate auto-allocated uid: %v", uids)
		}
	}
	if uids[0] < 1000 {
		t.Fatalf("expected auto uids >= 1000, got %v", uids)
	}
}

func TestApplyUsers_CreatesAccountFilesWhenMissing(t *testing.T) {
	// Minimal tar with no /etc/{passwd,shadow,group} at all (some scratch
	// images look like this). ApplyUsers should still produce a working
	// account triple.
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, []tarEntry{
		{hdr: tar.Header{Name: "bin/sh", Mode: 0o755, Typeflag: tar.TypeReg}, body: []byte("\x7fELF")},
	})

	if err := wsl.ApplyUsers(tarPath, []wsl.UserEntry{{Name: "solo", UID: 1000}}); err != nil {
		t.Fatalf("ApplyUsers: %v", err)
	}
	got := readTar(t, tarPath)
	for _, p := range []string{"etc/passwd", "etc/shadow", "etc/group"} {
		if _, ok := got[p]; !ok {
			t.Fatalf("expected %s to be created; entries: %v", p, keys(got))
		}
	}
	for _, p := range []string{"etc/", "home/", "home/solo/"} {
		e, ok := got[p]
		if !ok {
			t.Fatalf("expected %s directory entry to be created; entries: %v", p, keys(got))
		}
		if e.hdr.Typeflag != tar.TypeDir {
			t.Fatalf("expected %s to be a directory entry, got type %v", p, e.hdr.Typeflag)
		}
	}
	if findPasswdLine(got["etc/passwd"].body, "solo") == nil {
		t.Fatalf("solo missing from synthesised /etc/passwd: %q", got["etc/passwd"].body)
	}
	if findGroupLine(got["etc/group"].body, "solo") == nil {
		t.Fatalf("solo primary group missing: %q", got["etc/group"].body)
	}
	// Parent directory entries for the synthesized account files and the
	// home directory must also be present — wsl.exe --import requires
	// the parent dirs to exist before any child entry (see InjectCopies).
	for _, dir := range []string{"etc/", "home/"} {
		if _, ok := got[dir]; !ok {
			t.Fatalf("expected parent directory %q to be synthesized; entries: %v", dir, keys(got))
		}
	}
}

// TestApplyUsers_RewritesExistingHomeOwnership covers the case where the
// source rootfs tar already ships an explicit directory entry for the
// new user's home (e.g., upstream image baked in a default-user layer).
// The pre-existing entry is typically owned by root:root with mode
// 0755; without ApplyUsers rewriting it, the new user could not write
// into their own home on first login. The fix is to rewrite the
// existing dir header in-place during the first pass and suppress the
// duplicate trailing header in the second pass.
func TestApplyUsers_RewritesExistingHomeOwnership(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, []tarEntry{
		{hdr: tar.Header{Name: "etc/passwd", Mode: 0o644, Typeflag: tar.TypeReg},
			body: []byte("root:x:0:0:root:/root:/bin/sh\n")},
		{hdr: tar.Header{Name: "etc/shadow", Mode: 0o640, Typeflag: tar.TypeReg},
			body: []byte("root:*:::::::\n")},
		{hdr: tar.Header{Name: "etc/group", Mode: 0o644, Typeflag: tar.TypeReg},
			body: []byte("root:x:0:\n")},
		{hdr: tar.Header{Name: "home/", Mode: 0o755, Typeflag: tar.TypeDir}},
		// Pre-existing home dir owned by root:root with default mode.
		{hdr: tar.Header{Name: "home/alice/", Mode: 0o755, Uid: 0, Gid: 0, Typeflag: tar.TypeDir}},
	})

	if err := wsl.ApplyUsers(tarPath, []wsl.UserEntry{{Name: "alice", UID: 1500, GID: 1500}}); err != nil {
		t.Fatalf("ApplyUsers: %v", err)
	}
	got := readTar(t, tarPath)
	home, ok := got["home/alice/"]
	if !ok {
		t.Fatalf("home/alice/ entry missing; entries: %v", keys(got))
	}
	if home.hdr.Typeflag != tar.TypeDir {
		t.Fatalf("home/alice/ is not a directory entry, got type %v", home.hdr.Typeflag)
	}
	if home.hdr.Uid != 1500 || home.hdr.Gid != 1500 {
		t.Fatalf("home/alice/ ownership not rewritten: uid=%d gid=%d (want 1500:1500)", home.hdr.Uid, home.hdr.Gid)
	}
	if home.hdr.Mode != 0o700 {
		t.Fatalf("home/alice/ mode not rewritten: %o (want 0700)", home.hdr.Mode)
	}

	// Count dir entries for home/alice/ in the stream — must be exactly
	// one so tar consumers don't have to resolve last-write-wins.
	count := 0
	rd := readTarOrdered(t, tarPath)
	for _, e := range rd {
		if e.hdr.Name == "home/alice/" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one home/alice/ tar entry, got %d", count)
	}
}

// TestApplyUsers_RewritesImplicitHomeOwnership covers the case where the
// source tar ships a *child* file under the new user's home (e.g., a
// pre-seeded .bashrc) but no explicit directory entry for the home
// itself. Without an authoritative trailing dir header, wsl.exe
// --import would create the implicit parent with default 0755 root:root
// ownership and the user could not write to their own home.
func TestApplyUsers_RewritesImplicitHomeOwnership(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, []tarEntry{
		{hdr: tar.Header{Name: "etc/passwd", Mode: 0o644, Typeflag: tar.TypeReg},
			body: []byte("root:x:0:0:root:/root:/bin/sh\n")},
		{hdr: tar.Header{Name: "etc/shadow", Mode: 0o640, Typeflag: tar.TypeReg},
			body: []byte("root:*:::::::\n")},
		{hdr: tar.Header{Name: "etc/group", Mode: 0o644, Typeflag: tar.TypeReg},
			body: []byte("root:x:0:\n")},
		// Child file under the home with no explicit home dir entry.
		{hdr: tar.Header{Name: "home/alice/.bashrc", Mode: 0o644, Uid: 0, Gid: 0, Typeflag: tar.TypeReg},
			body: []byte("# seeded\n")},
	})

	if err := wsl.ApplyUsers(tarPath, []wsl.UserEntry{{Name: "alice", UID: 1500, GID: 1500}}); err != nil {
		t.Fatalf("ApplyUsers: %v", err)
	}
	got := readTar(t, tarPath)
	home, ok := got["home/alice/"]
	if !ok {
		t.Fatalf("home/alice/ entry missing (must be emitted even when only implied by child files); entries: %v", keys(got))
	}
	if home.hdr.Typeflag != tar.TypeDir {
		t.Fatalf("home/alice/ is not a directory entry, got type %v", home.hdr.Typeflag)
	}
	if home.hdr.Uid != 1500 || home.hdr.Gid != 1500 {
		t.Fatalf("home/alice/ ownership: uid=%d gid=%d (want 1500:1500)", home.hdr.Uid, home.hdr.Gid)
	}
	if home.hdr.Mode != 0o700 {
		t.Fatalf("home/alice/ mode: %o (want 0700)", home.hdr.Mode)
	}
	// The seeded child must survive untouched.
	if _, ok := got["home/alice/.bashrc"]; !ok {
		t.Fatalf("home/alice/.bashrc child missing; entries: %v", keys(got))
	}
}

// readTarOrdered returns the entries in the order they appear so a test
// can assert there's exactly one header per path.
func readTarOrdered(t *testing.T, path string) []tarEntry {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	tr := tar.NewReader(f)
	var out []tarEntry
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		out = append(out, tarEntry{hdr: *hdr})
	}
	return out
}

func TestApplyUsers_PreservesOriginalHeaderMode(t *testing.T) {
	// The substituted /etc/passwd should keep its original mode, not be
	// silently rewritten with our default.
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, []tarEntry{
		{hdr: tar.Header{Name: "etc/passwd", Mode: 0o600, Typeflag: tar.TypeReg},
			body: []byte("root:x:0:0::/root:/bin/sh\n")},
	})
	if err := wsl.ApplyUsers(tarPath, []wsl.UserEntry{{Name: "x"}}); err != nil {
		t.Fatal(err)
	}
	got := readTar(t, tarPath)
	if got["etc/passwd"].hdr.Mode != 0o600 {
		t.Fatalf("expected /etc/passwd mode 0600 to be preserved, got %o", got["etc/passwd"].hdr.Mode)
	}
}

// --- small local helpers ---

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func keys(m map[string]tarEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			t.Fatalf("not numeric: %q", s)
		}
		n = n*10 + int(r-'0')
	}
	return n
}
