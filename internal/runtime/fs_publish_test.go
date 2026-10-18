package runtime

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sync"
	"testing"
)

func TestPublishAtomicFileFlushesAndRefusesReplacement(t *testing.T) {
	home := t.TempDir()
	root, err := OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.EnsurePrivateDirs("archive"); err != nil {
		t.Fatal(err)
	}
	first, err := root.PublishAtomicFile("archive/sealed.bin", []byte("sealed payload"), 0o600)
	if !first.Published || !first.FileFlushed {
		t.Fatalf("publication did not report file and destination state: result=%+v err=%v", first, err)
	}
	if err != nil && !errors.Is(err, ErrDirectoryFlushUnsupported) {
		t.Fatalf("unexpected post-publication error: result=%+v err=%v", first, err)
	}
	if err == nil && !first.DirectoryFlushed {
		t.Fatalf("successful publication lacks directory flush evidence: %+v", first)
	}
	if errors.Is(err, ErrDirectoryFlushUnsupported) {
		t.Logf("directory flush unsupported or unconfirmed on %s; file flush and no-replace publication remain verified", goruntime.GOOS)
	}
	second, err := root.PublishAtomicFile("archive/sealed.bin", []byte("replacement"), 0o600)
	if !errors.Is(err, ErrAlreadyPublished) || second.Published {
		t.Fatalf("existing destination was replaced: result=%+v err=%v", second, err)
	}
	file, err := root.OpenRegularFile("archive/sealed.bin")
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || string(data) != "sealed payload" {
		t.Fatalf("published payload readback=%q read=%v close=%v", data, readErr, closeErr)
	}
	if result, err := root.PublishAtomicFile("../outside.bin", []byte("escape"), 0o600); !errors.Is(err, ErrPathDenied) || result.Published {
		t.Fatalf("publication escaped verified root: result=%+v err=%v", result, err)
	}
	rootResult, rootErr := root.PublishAtomicFile("root-level.bin", []byte("root payload"), 0o600)
	if !rootResult.Published || !rootResult.FileFlushed {
		t.Fatalf("root-level publication did not flush its payload: result=%+v err=%v", rootResult, rootErr)
	}
	if rootErr != nil && !errors.Is(rootErr, ErrDirectoryFlushUnsupported) {
		t.Fatalf("root-level publication failed unexpectedly: result=%+v err=%v", rootResult, rootErr)
	}
}

func TestPublishAtomicFileNoReplaceIsRaceSafe(t *testing.T) {
	home := t.TempDir()
	root, err := OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.EnsurePrivateDirs("archive"); err != nil {
		t.Fatal(err)
	}
	const writers = 8
	results := make([]AtomicPublishResult, writers)
	errs := make([]error, writers)
	var group sync.WaitGroup
	for index := 0; index < writers; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			results[index], errs[index] = root.PublishAtomicFile("archive/race.bin", []byte{byte(index)}, 0o600)
		}(index)
	}
	group.Wait()
	winners := 0
	for index, result := range results {
		if result.Published {
			winners++
			if !result.FileFlushed {
				t.Fatalf("writer %d published without flushing the file: %+v", index, result)
			}
			if errs[index] != nil && !errors.Is(errs[index], ErrDirectoryFlushUnsupported) {
				t.Fatalf("winner %d had an unrelated error: %v", index, errs[index])
			}
		} else if !errors.Is(errs[index], ErrAlreadyPublished) {
			t.Fatalf("loser %d did not observe the no-replace conflict: result=%+v err=%v", index, result, errs[index])
		}
	}
	if winners != 1 {
		t.Fatalf("no-replace publication had %d winners, want exactly one: %+v", winners, results)
	}
	if info, err := os.Stat(filepath.Join(home, "archive", "race.bin")); err != nil || info.Size() != 1 {
		t.Fatalf("racing publication left unexpected target: info=%v err=%v", info, err)
	}
}
