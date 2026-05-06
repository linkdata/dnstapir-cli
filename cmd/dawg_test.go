package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/smhanov/dawg"
)

func TestParseCSVNamesKeepsHeaderlessFirstRowAndNormalizesBeforeSort(t *testing.T) {
	src := writeTempFile(t, "tranco.csv", "1,nwedothisallyearu.xyz\n2,_wildcard_.com.ph\n3,Google.COM\n4,google.com.\n")

	got, err := parseCSVNames(src)
	if err != nil {
		t.Fatalf("parseCSVNames() error = %v", err)
	}

	want := []string{
		"_wildcard_.com.ph.",
		"google.com.",
		"nwedothisallyearu.xyz.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseCSVNames() = %#v, want %#v", got, want)
	}
}

func TestParseCSVNamesSkipsRankDomainHeader(t *testing.T) {
	src := writeTempFile(t, "domains.csv", "rank,domain\n1,Example.COM\n")

	got, err := parseCSVNames(src)
	if err != nil {
		t.Fatalf("parseCSVNames() error = %v", err)
	}

	want := []string{"example.com."}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseCSVNames() = %#v, want %#v", got, want)
	}
}

func TestCompileTextStreamsSortedNamesAndSkipsNormalizedDuplicates(t *testing.T) {
	src := writeTempFile(t, "forward_new", ".aaa.nic\n.aaa.nic.\ngoogle.com\n")
	out := filepath.Join(t.TempDir(), "forward_new.dawg")

	if err := compileDawg("text", src, out); err != nil {
		t.Fatalf("compileDawg() error = %v", err)
	}

	dawgf, err := dawg.Load(out)
	if err != nil {
		t.Fatalf("dawg.Load() error = %v", err)
	}
	defer dawgf.Close()

	if got, want := dawgf.NumAdded(), 2; got != want {
		t.Fatalf("NumAdded() = %d, want %d", got, want)
	}
	for _, name := range []string{".aaa.nic.", "google.com."} {
		if idx := dawgf.IndexOf(name); idx == -1 {
			t.Fatalf("IndexOf(%q) = -1, want match", name)
		}
	}
}

func TestCompileTextSortsSmallUnsortedSourceInMemory(t *testing.T) {
	src := writeTempFile(t, "domains.txt", "z.example\na.example\n")
	out := filepath.Join(t.TempDir(), "domains.dawg")

	if err := compileDawg("text", src, out); err != nil {
		t.Fatalf("compileDawg() error = %v", err)
	}

	dawgf, err := dawg.Load(out)
	if err != nil {
		t.Fatalf("dawg.Load() error = %v", err)
	}
	defer dawgf.Close()

	for _, name := range []string{"a.example.", "z.example."} {
		if idx := dawgf.IndexOf(name); idx == -1 {
			t.Fatalf("IndexOf(%q) = -1, want match", name)
		}
	}
}

func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	return path
}
