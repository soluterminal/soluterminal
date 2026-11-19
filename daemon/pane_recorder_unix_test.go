//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestRecordingFolderAndFileModesWhateverTheUmask(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	tp := newRecTestPane(t)
	rec := tp.start()
	tp.send(FrameData, []byte("x"))
	tp.sync()
	tp.m.StopRecording(tp.p, "local human")
	waitRecorder(t, rec)
	root := filepath.Join(tp.data, recordingsDirName)
	for path, want := range map[string]os.FileMode{root: 0o700, filepath.Join(root, tp.p.ID): 0o700, castFiles(t, tp.data, tp.p.ID)[0]: 0o600} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s: %o, want %o", path, info.Mode().Perm(), want)
		}
	}
}

func TestRecordingRefusesUnsafeFolders(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, data, paneID string){
		"recordings is a symlink": func(t *testing.T, data, _ string) {
			if err := os.Symlink(t.TempDir(), filepath.Join(data, recordingsDirName)); err != nil {
				t.Fatal(err)
			}
		},
		"pane folder is a symlink": func(t *testing.T, data, paneID string) {
			if err := os.Mkdir(filepath.Join(data, recordingsDirName), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), filepath.Join(data, recordingsDirName, paneID)); err != nil {
				t.Fatal(err)
			}
		},
		"recordings is group-readable": func(t *testing.T, data, _ string) {
			if err := os.Mkdir(filepath.Join(data, recordingsDirName), 0o750); err != nil {
				t.Fatal(err)
			}
			_ = os.Chmod(filepath.Join(data, recordingsDirName), 0o750)
		},
		"pane folder is world-readable": func(t *testing.T, data, paneID string) {
			if err := os.MkdirAll(filepath.Join(data, recordingsDirName, paneID), 0o700); err != nil {
				t.Fatal(err)
			}
			// A file keeps the folder through the sweep, which removes
			// empty pane folders (a fresh one is then made 0700).
			if err := os.WriteFile(filepath.Join(data, recordingsDirName, paneID, "20261118T000000Z.cast"), []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_ = os.Chmod(filepath.Join(data, recordingsDirName, paneID), 0o705)
		},
	} {
		t.Run(name, func(t *testing.T) {
			tp := newRecTestPane(t)
			setup(t, tp.data, tp.p.ID)
			_, err := tp.m.StartRecording(tp.p, "local")
			if !errors.Is(err, errRecordingFolder) {
				t.Fatalf("switch-on: %v", err)
			}
			if tp.p.recording.active.Load() != nil {
				t.Fatal("recording on in a refused folder")
			}
			if d := tp.p.Recording(); d == nil || d.StoppedReason != recordingStopFolder {
				t.Fatalf("indicator %+v", d)
			}
		})
	}
}

func TestRecordingFileIsNeverOpenedThroughASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere")
	link := filepath.Join(dir, "20261119T000000Z.cast")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := openNewRecordingFile(link); err == nil {
		t.Fatal("created through a symlink")
	}
	if _, err := openExistingRecordingFile(link); err == nil {
		t.Fatal("reopened through a symlink")
	}
	if exists(target) {
		t.Fatal("the symlink target was created")
	}
}
