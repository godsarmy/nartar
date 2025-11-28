package main

import (
	"archive/tar"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nix-community/go-nix/pkg/nar"
)

const (
	dirMode      int64 = 0o555
	fileMode     int64 = 0o444
	execFileMode int64 = 0o555
	symlinkMode  int64 = 0o777
)

var zeroTime = time.Unix(0, 0)

type tarEntry struct {
	path       string
	kind       byte
	linkTarget string
	data       []byte
	executable bool
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
	}

	switch os.Args[1] {
	case "nar2tar":
		if err := runNarToTar(os.Args[2:]); err != nil {
			exitErr(err)
		}
	case "tar2nar":
		if err := runTarToNar(os.Args[2:]); err != nil {
			exitErr(err)
		}
	case "-h", "--help", "help":
		printUsage()
	default:
		exitErr(fmt.Errorf("unknown command %q", os.Args[1]))
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "Usage:\n")
	fmt.Fprintf(os.Stderr, "  nartar nar2tar -i input.nar -o output.tar\n")
	fmt.Fprintf(os.Stderr, "  nartar tar2nar -i input.tar -o output.nar\n")
	fmt.Fprintf(os.Stderr, "Use '-' for stdin/stdout. Timestamps are normalized to the Unix epoch.\n")
	os.Exit(2)
}

func runNarToTar(args []string) error {
	fs := flag.NewFlagSet("nar2tar", flag.ContinueOnError)
	input := fs.String("i", "-", "input NAR file ('-' for stdin)")
	output := fs.String("o", "-", "output tar file ('-' for stdout)")
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}

	in, err := openInput(*input)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := openOutput(*output)
	if err != nil {
		return err
	}
	defer out.Close()

	return narToTar(in, out)
}

func runTarToNar(args []string) error {
	fs := flag.NewFlagSet("tar2nar", flag.ContinueOnError)
	input := fs.String("i", "-", "input tar file ('-' for stdin)")
	output := fs.String("o", "-", "output NAR file ('-' for stdout)")
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}

	in, err := openInput(*input)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := openOutput(*output)
	if err != nil {
		return err
	}
	defer out.Close()

	return tarToNar(in, out)
}

func openInput(name string) (io.ReadCloser, error) {
	if name == "" || name == "-" {
		return io.NopCloser(os.Stdin), nil
	}

	return os.Open(name)
}

type nopWriteCloser struct {
	io.Writer
}

func (n nopWriteCloser) Close() error { return nil }

func openOutput(name string) (io.WriteCloser, error) {
	if name == "" || name == "-" {
		return nopWriteCloser{Writer: os.Stdout}, nil
	}

	return os.Create(name)
}

func narToTar(in io.Reader, out io.Writer) error {
	nr, err := nar.NewReader(in)
	if err != nil {
		return fmt.Errorf("opening nar: %w", err)
	}
	defer nr.Close()

	tw := tar.NewWriter(out)
	defer tw.Close()

	for {
		hdr, err := nr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return fmt.Errorf("reading nar header: %w", err)
		}

		name, skip := tarPathForNarHeader(hdr)
		if skip {
			continue
		}

		switch hdr.Type {
		case nar.TypeDirectory:
			if !strings.HasSuffix(name, "/") {
				name += "/"
			}

			th := &tar.Header{
				Name:     name,
				Mode:     dirMode,
				ModTime:  zeroTime,
				Typeflag: tar.TypeDir,
			}

			if err := tw.WriteHeader(th); err != nil {
				return fmt.Errorf("writing tar dir header: %w", err)
			}
		case nar.TypeSymlink:
			th := &tar.Header{
				Name:     name,
				Mode:     symlinkMode,
				Linkname: filepath.ToSlash(hdr.LinkTarget),
				ModTime:  zeroTime,
				Typeflag: tar.TypeSymlink,
			}

			if err := tw.WriteHeader(th); err != nil {
				return fmt.Errorf("writing tar symlink header: %w", err)
			}
		case nar.TypeRegular:
			th := &tar.Header{
				Name:     name,
				Mode:     pickFileMode(hdr.Executable),
				Size:     hdr.Size,
				ModTime:  zeroTime,
				Typeflag: tar.TypeReg,
			}

			if err := tw.WriteHeader(th); err != nil {
				return fmt.Errorf("writing tar file header: %w", err)
			}

			if _, err := io.CopyN(tw, nr, hdr.Size); err != nil {
				return fmt.Errorf("copying file content: %w", err)
			}
		default:
			return fmt.Errorf("unsupported nar node type %q", hdr.Type)
		}
	}

	return tw.Close()
}

func tarToNar(in io.Reader, out io.Writer) error {
	tr := tar.NewReader(in)

	entries := make(map[string]*tarEntry)

	for {
		th, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return fmt.Errorf("reading tar: %w", err)
		}

		p, skip, err := normalizeTarPath(th.Name)
		if err != nil {
			return fmt.Errorf("invalid tar entry path %q: %w", th.Name, err)
		}

		if skip {
			continue
		}

		ensureParentDirs(p, entries)

		switch th.Typeflag {
		case tar.TypeDir:
			entries[p] = &tarEntry{path: p, kind: tar.TypeDir}
		case tar.TypeSymlink:
			entries[p] = &tarEntry{
				path:       p,
				kind:       tar.TypeSymlink,
				linkTarget: filepath.ToSlash(th.Linkname),
			}
		case tar.TypeReg, tar.TypeRegA:
			data, err := io.ReadAll(tr)
			if err != nil {
				return fmt.Errorf("reading tar file %q: %w", th.Name, err)
			}

			executable := th.FileInfo().Mode()&0o111 != 0

			entries[p] = &tarEntry{
				path:       p,
				kind:       tar.TypeReg,
				data:       data,
				executable: executable,
			}
		case tar.TypeXHeader, tar.TypeXGlobalHeader, tar.TypeGNULongLink, tar.TypeGNULongName:
			// Ignore extended headers we don't need for NAR data.
		default:
			return fmt.Errorf("unsupported tar entry %q with type %v", th.Name, th.Typeflag)
		}
	}

	rootEntry := entries["/"]

	paths := make([]string, 0, len(entries))
	for p := range entries {
		if p != "/" {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	nw, err := nar.NewWriter(out)
	if err != nil {
		return fmt.Errorf("creating nar writer: %w", err)
	}

	if rootEntry != nil {
		if rootEntry.kind != tar.TypeDir && len(paths) > 0 {
			return fmt.Errorf("root file with additional entries is not supported")
		}

		if err := writeNarEntry(nw, rootEntry); err != nil {
			return fmt.Errorf("writing nar root: %w", err)
		}
	} else {
		if err := nw.WriteHeader(&nar.Header{Path: "/", Type: nar.TypeDirectory}); err != nil {
			return fmt.Errorf("writing nar root: %w", err)
		}
	}

	for _, p := range paths {
		entry := entries[p]
		if entry == nil {
			continue
		}

		if err := writeNarEntry(nw, entry); err != nil {
			return fmt.Errorf("writing nar for %q: %w", p, err)
		}
	}

	return nw.Close()
}

func normalizeTarPath(name string) (string, bool, error) {
	name = filepath.ToSlash(name)

	if strings.Contains(name, "\x00") {
		return "", false, fmt.Errorf("path contains null byte")
	}

	name = strings.TrimPrefix(name, "./")

	trimmed := strings.TrimPrefix(name, "/")
	if trimmed == "" || trimmed == "." {
		return "", true, nil
	}

	if !strings.HasPrefix(trimmed, "-") {
		return "", true, nil
	}

	trimmed = strings.TrimPrefix(trimmed, "-")
	trimmed = strings.TrimPrefix(trimmed, "/")

	clean := path.Clean("/" + trimmed)

	if clean == "/" && trimmed != "" {
		return "", false, fmt.Errorf("invalid normalized path")
	}

	if strings.HasPrefix(clean, "/..") || strings.Contains(clean, "/../") {
		return "", false, fmt.Errorf("path attempts to escape root")
	}

	return clean, false, nil
}

func ensureParentDirs(p string, entries map[string]*tarEntry) {
	dir := path.Dir(p)
	for dir != "/" && dir != "." {
		if _, ok := entries[dir]; !ok {
			entries[dir] = &tarEntry{path: dir, kind: tar.TypeDir}
		}

		dir = path.Dir(dir)
	}
}

func pickFileMode(exec bool) int64 {
	if exec {
		return execFileMode
	}

	return fileMode
}

func tarPathForNarHeader(hdr *nar.Header) (string, bool) {
	p := filepath.ToSlash(hdr.Path)

	if p == "/" {
		if hdr.Type == nar.TypeRegular {
			return "-", false
		}

		return "", true
	}

	trimmed := strings.TrimPrefix(p, "/")
	if trimmed == "" {
		return "", true
	}

	return path.Join("-", trimmed), false
}

func writeNarEntry(nw *nar.Writer, entry *tarEntry) error {
	switch entry.kind {
	case tar.TypeDir:
		return nw.WriteHeader(&nar.Header{Path: entry.path, Type: nar.TypeDirectory})
	case tar.TypeSymlink:
		return nw.WriteHeader(&nar.Header{
			Path:       entry.path,
			Type:       nar.TypeSymlink,
			LinkTarget: entry.linkTarget,
		})
	case tar.TypeReg:
		h := &nar.Header{
			Path:       entry.path,
			Type:       nar.TypeRegular,
			Size:       int64(len(entry.data)),
			Executable: entry.executable,
		}

		if err := nw.WriteHeader(h); err != nil {
			return err
		}

		_, err := nw.Write(entry.data)
		return err
	default:
		return fmt.Errorf("unsupported entry type %v", entry.kind)
	}
}

func exitErr(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}
