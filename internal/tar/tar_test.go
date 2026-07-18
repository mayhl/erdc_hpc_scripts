package tar

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsArchive(t *testing.T) {
	yes := []string{"foo.tar", "foo.tar.gz", "foo.tgz"}
	no := []string{"foo", "foo/", "foo.txt", "dir.tar.bak"}
	for _, p := range yes {
		if !isArchive(p) {
			t.Errorf("isArchive(%q) = false, want true", p)
		}
	}
	for _, p := range no {
		if isArchive(p) {
			t.Errorf("isArchive(%q) = true, want false", p)
		}
	}
}

func TestArchiveName(t *testing.T) {
	if got := archiveName("mydir/", false); got != "mydir.tar" {
		t.Errorf("plain = %q", got)
	}
	if got := archiveName("mydir", true); got != "mydir.tar.gz" {
		t.Errorf("gzip = %q", got)
	}
}

// TestRoundTrip archives a dir and extracts it back with the real tar binary,
// exercising the full pipe + meter path (plain and gzip).
func TestRoundTrip(t *testing.T) {
	for _, gz := range []bool{false, true} {
		name := "plain"
		if gz {
			name = "gzip"
		}
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			src := filepath.Join(base, "src")
			if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(src, "sub", "a.txt"), []byte("hello world"), 0o644); err != nil {
				t.Fatal(err)
			}

			t.Chdir(base)
			if rc := Run("src", gz); rc != 0 {
				t.Fatalf("create rc=%d", rc)
			}
			archive := archiveName("src", gz)
			if _, err := os.Stat(archive); err != nil {
				t.Fatalf("archive not created: %v", err)
			}

			// Extract into a fresh dir and compare content.
			out := filepath.Join(base, "out")
			if err := os.MkdirAll(out, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(base, archive), filepath.Join(out, archive)); err != nil {
				t.Fatal(err)
			}
			t.Chdir(out)
			if rc := Run(archive, false); rc != 0 {
				t.Fatalf("extract rc=%d", rc)
			}
			got, err := os.ReadFile(filepath.Join(out, "src", "sub", "a.txt"))
			if err != nil || string(got) != "hello world" {
				t.Fatalf("round-trip content=%q err=%v", got, err)
			}
		})
	}
}

func TestCreateRooted(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "deep", "case_a_250")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "out.nc"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	staging := filepath.Join(base, "deep", "250.tar")
	if rc := CreateRooted(src, staging); rc != 0 {
		t.Fatalf("create rc=%d", rc)
	}
	// members are rooted at the dir's basename: extracting elsewhere recreates
	// case_a_250/ exactly, not deep/case_a_250/
	out := filepath.Join(base, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staging, filepath.Join(out, "250.tar")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(out)
	if rc := Run("250.tar", false); rc != 0 {
		t.Fatalf("extract rc=%d", rc)
	}
	got, err := os.ReadFile(filepath.Join(out, "case_a_250", "out.nc"))
	if err != nil || string(got) != "data" {
		t.Fatalf("round-trip content=%q err=%v", got, err)
	}
}

func TestCreateRootedSubset(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "deep", "case_a_250")
	if err := os.MkdirAll(filepath.Join(src, "output"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"output/a.nc", "output/b.nc", "log.out"} {
		if err := os.WriteFile(filepath.Join(src, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// pack only the two output members — the log stays out of this chunk
	staging := filepath.Join(base, "deep", "chunk.tar")
	if rc := CreateRootedSubset(src, staging, []string{"output/a.nc", "output/b.nc"}); rc != 0 {
		t.Fatalf("create rc=%d", rc)
	}
	out := filepath.Join(base, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staging, filepath.Join(out, "chunk.tar")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(out)
	if rc := Run("chunk.tar", false); rc != 0 {
		t.Fatalf("extract rc=%d", rc)
	}
	// the subset extracts rooted at the leaf basename — case_a_250/output/{a,b}.nc only
	if got, err := os.ReadFile(filepath.Join(out, "case_a_250", "output", "a.nc")); err != nil || string(got) != "output/a.nc" {
		t.Fatalf("member content=%q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(out, "case_a_250", "log.out")); !os.IsNotExist(err) {
		t.Fatalf("log.out should not be in the subset chunk (err=%v)", err)
	}
}
