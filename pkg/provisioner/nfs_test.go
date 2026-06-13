package provisioner

import (
	"strings"
	"testing"
)

func TestValidateNFSExportPathRejectsUnsafeCharacters(t *testing.T) {
	unsafePaths := []string{
		"/srv/nfs/app;touch",
		"/srv/nfs/app with space",
		"/srv/nfs/app$(touch)",
		"/srv/nfs/app`touch`",
		"/srv/nfs/app|cat",
		"/srv/nfs/app\nnext",
		"/srv/nfs/app'quote",
	}

	for _, path := range unsafePaths {
		t.Run(path, func(t *testing.T) {
			if err := ValidateNFSExportPath(path); err == nil {
				t.Fatalf("expected %q to be rejected", path)
			}
		})
	}
}

func TestValidateNFSExportPathAllowsSafePath(t *testing.T) {
	if err := ValidateNFSExportPath("/srv/tako-nfs/shared_repo-1"); err != nil {
		t.Fatalf("expected safe NFS path to pass: %v", err)
	}
}

func TestValidateNFSExportOptionsRejectsUnsafeCharacters(t *testing.T) {
	unsafeOptions := [][]string{
		{"rw,no_subtree_check"},
		{"rw) *(rw"},
		{"rw\n/srv/other"},
		{""},
	}

	for _, options := range unsafeOptions {
		t.Run(strings.Join(options, ","), func(t *testing.T) {
			if err := ValidateNFSExportOptions(options); err == nil {
				t.Fatalf("expected %v to be rejected", options)
			}
		})
	}
}

func TestValidateNFSExportOptionsAllowsKnownOptions(t *testing.T) {
	options := []string{"rw", "sync", "no_subtree_check", "no_root_squash", "anonuid=1000", "sec=sys"}
	if err := ValidateNFSExportOptions(options); err != nil {
		t.Fatalf("expected safe NFS options to pass: %v", err)
	}
}

func TestNFSCommandBuildersQuoteConfiguredValues(t *testing.T) {
	exportPath := "/srv/nfs/app;touch"
	if got := exportExistsCommand(exportPath); !strings.Contains(got, "p='/srv/nfs/app;touch'") {
		t.Fatalf("export exists command did not quote export path: %s", got)
	}
	if got := removeExportsLineCommand(exportPath); !strings.Contains(got, "p='/srv/nfs/app;touch'") ||
		!strings.Contains(got, "tmp=$(mktemp)") ||
		!strings.Contains(got, "sudo install -m 0644 -o root -g root") ||
		strings.Contains(got, "tee /etc/exports >") ||
		strings.Contains(got, "> '/etc/exports'") {
		t.Fatalf("remove exports command did not quote export path: %s", got)
	}

	mountPoint := "/mnt/tako-nfs/app;touch"
	if got := removeFstabEntryCommand(mountPoint); !strings.Contains(got, "p='/mnt/tako-nfs/app;touch'") ||
		!strings.Contains(got, "tmp=$(mktemp)") ||
		!strings.Contains(got, "sudo install -m 0644 -o root -g root") ||
		strings.Contains(got, "tee /etc/fstab >") ||
		strings.Contains(got, "> '/etc/fstab'") {
		t.Fatalf("remove fstab command did not quote mount point: %s", got)
	}

	line := "10.0.0.1:/srv/nfs/app /mnt/tako-nfs/app nfs4 hard 0 0"
	if got := appendRootOwnedLineCommand("/etc/fstab", line); !strings.Contains(got, "'10.0.0.1:/srv/nfs/app /mnt/tako-nfs/app nfs4 hard 0 0'") {
		t.Fatalf("append line command did not quote line: %s", got)
	}
}
