package readonly

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/fs/walk"
)

// tree records every path under dir with its size, time and contents.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		v := info.Mode().String() + " " + info.ModTime().String()
		if !info.IsDir() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			v += " " + string(b)
		}
		out[rel] = v
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEveryWriteIsRefused(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for p, b := range map[string]string{"A/01.flac": "one", "A/02.flac": "two", "B/03.mp3": "three"} {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "Empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := tree(t, dir)
	inner, err := fs.NewFs(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	r := Wrap(inner)

	// Reading works.
	var files []string
	var listed []fs.Object
	err = walk.ListR(ctx, r, "", true, -1, walk.ListObjects, func(entries fs.DirEntries) error {
		for _, e := range entries {
			files = append(files, e.Remote())
			if o, ok := e.(fs.Object); ok {
				listed = append(listed, o)
			}
		}
		return nil
	})
	if err != nil || len(files) != 3 {
		t.Fatalf("listed %v (%v)", files, err)
	}
	o, err := r.NewObject(ctx, "B/03.mp3")
	if err != nil {
		t.Fatal(err)
	}
	rc, err := o.Open(ctx, &fs.RangeOption{Start: 1, End: 3})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "hre" {
		t.Errorf("range read %q", got)
	}

	// No optional method of the backend is offered, writer or not,
	// except Shutdown.
	fv := reflect.ValueOf(*r.Features())
	for i := range fv.NumField() {
		name := fv.Type().Field(i).Name
		if fv.Field(i).Kind() == reflect.Func && !fv.Field(i).IsNil() && name != "Shutdown" {
			t.Errorf("Features().%s is offered", name)
		}
	}

	// Every write, directly and through rclone's operations.
	refused := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrReadOnly) {
			t.Errorf("%s: %v, want ErrReadOnly", what, err)
		}
	}
	src := object.NewStaticObjectInfo("new.flac", time.Now(), 3, true, nil, nil)
	_, err = r.Put(ctx, strings.NewReader("new"), src)
	refused("Put", err)
	refused("Mkdir", r.Mkdir(ctx, "New"))
	refused("Rmdir", r.Rmdir(ctx, "Empty"))
	refused("SetModTime", o.SetModTime(ctx, time.Now()))
	refused("Update", o.Update(ctx, bytes.NewReader([]byte("x")), src))
	refused("Remove", o.Remove(ctx))
	_, err = operations.RcatSize(ctx, r, "rcat.flac", io.NopCloser(strings.NewReader("abc")), 3, time.Now(), nil)
	refused("RcatSize", err)
	_, err = operations.Rcat(ctx, r, "rcat2.flac", io.NopCloser(strings.NewReader("abc")), time.Now(), nil)
	refused("Rcat", err)
	_, err = operations.Copy(ctx, r, nil, "A/copy.flac", o)
	refused("Copy", err)
	_, err = operations.Move(ctx, r, nil, "A/moved.mp3", o)
	refused("Move", err)
	refused("DeleteFile", operations.DeleteFile(ctx, o))
	refused("Mkdir via operations", operations.Mkdir(ctx, r, "New2"))
	refused("Rmdir via operations", operations.Rmdir(ctx, r, "Empty"))
	// Purge deletes file by file, each refused; it reports a count.
	if err := operations.Purge(ctx, r, "A"); err == nil {
		t.Error("Purge succeeded")
	}
	// Files found by listing are read-only too.
	for _, l := range listed {
		if _, ok := l.(*Object); !ok || l.Fs() != fs.Info(r) {
			t.Errorf("listed %s is a %T on %v", l.Remote(), l, l.Fs())
		}
		refused("Remove of a listed file", l.Remove(ctx))
		refused("SetModTime of a listed file", l.SetModTime(ctx, time.Now()))
	}
	if !reflect.DeepEqual(tree(t, dir), before) {
		t.Errorf("the folder changed:\nbefore %v\nafter  %v", before, tree(t, dir))
	}
}
