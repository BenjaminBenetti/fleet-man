package binstore

import (
	"archive/tar"
	"compress/gzip"
	"debug/elf"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
)

// TmuxBinary is where the bundle keeps tmux, relative to its root (the
// bundle unpacks into /opt/fleet/tmux, the terminfo search path compiled into
// that tmux).
const TmuxBinary = "bin/tmux"

// maxTmuxBundle caps the bundle the store accepts.
const maxTmuxBundle = 64 << 20

// checkTmuxBundle vets the tmux bundle at p for arch: a gzip tar holding
// TmuxBinary as a linux ELF executable for arch, with no entry outside the
// bundle's root.
func checkTmuxBundle(p, arch string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(io.LimitReader(f, maxTmuxBundle))
	if err != nil {
		return fmt.Errorf("%s is not a gzip tmux bundle: %w", p, err)
	}
	tr := tar.NewReader(zr)
	found := false
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%s is not a tmux bundle: %w", p, err)
		}
		name := path.Clean(h.Name)
		if path.IsAbs(name) || name == ".." || len(name) > 2 && name[:3] == "../" {
			return fmt.Errorf("%s has an entry outside its root: %q", p, h.Name)
		}
		if name != TmuxBinary {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return fmt.Errorf("%s: %s is not a regular file", p, TmuxBinary)
		}
		head := make([]byte, 20)
		if _, err := io.ReadFull(tr, head); err != nil {
			return fmt.Errorf("%s: %s is not an ELF executable", p, TmuxBinary)
		}
		machine, osabi, err := elfHeader(head)
		if err != nil {
			return fmt.Errorf("%s: %s: %w", p, TmuxBinary, err)
		}
		if want := elfMachine(arch); machine != want || osabi != elf.ELFOSABI_NONE && osabi != elf.ELFOSABI_LINUX {
			return fmt.Errorf("%s: %s is for %s, not linux/%s", p, TmuxBinary, machine, arch)
		}
		found = true
	}
	if !found {
		return fmt.Errorf("%s has no %s", p, TmuxBinary)
	}
	return nil
}

func elfMachine(arch string) elf.Machine {
	switch arch {
	case "amd64":
		return elf.EM_X86_64
	case "arm64":
		return elf.EM_AARCH64
	case "386":
		return elf.EM_386
	case "arm":
		return elf.EM_ARM
	case "riscv64":
		return elf.EM_RISCV
	case "ppc64le":
		return elf.EM_PPC64
	case "s390x":
		return elf.EM_S390
	}
	return elf.EM_NONE
}

// elfHeader reads the machine and OS ABI of an ELF file's first 20 bytes.
func elfHeader(b []byte) (elf.Machine, elf.OSABI, error) {
	if len(b) < 20 || string(b[:4]) != elf.ELFMAG {
		return 0, 0, errors.New("not an ELF executable")
	}
	var order binary.ByteOrder
	switch elf.Data(b[elf.EI_DATA]) {
	case elf.ELFDATA2LSB:
		order = binary.LittleEndian
	case elf.ELFDATA2MSB:
		order = binary.BigEndian
	default:
		return 0, 0, errors.New("ELF file of unknown byte order")
	}
	return elf.Machine(order.Uint16(b[18:20])), elf.OSABI(b[elf.EI_OSABI]), nil
}
