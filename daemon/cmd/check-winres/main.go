// check-winres verifies the actual linked PE resource directory, not loose
// strings elsewhere in the executable. It runs on any Go host.
package main

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf16"
)

func main() {
	if len(os.Args) != 3 {
		fail("usage: check-winres EXE X.Y.Z")
	}
	if err := check(os.Args[1], os.Args[2]); err != nil {
		fail("%v", err)
	}
	fmt.Println("Windows resources verified:", os.Args[1], os.Args[2])
}
func fail(s string, args ...any) { fmt.Fprintf(os.Stderr, s+"\n", args...); os.Exit(1) }
func wide(s string) []byte {
	var b bytes.Buffer
	for _, u := range utf16.Encode([]rune(s + "\x00")) {
		_ = binary.Write(&b, binary.LittleEndian, u)
	}
	return b.Bytes()
}
func check(name, version string) error {
	f, err := pe.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	var rva uint32
	switch h := f.OptionalHeader.(type) {
	case *pe.OptionalHeader64:
		rva = h.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_RESOURCE].VirtualAddress
	case *pe.OptionalHeader32:
		rva = h.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_RESOURCE].VirtualAddress
	}
	var data []byte
	for _, s := range f.Sections {
		if rva >= s.VirtualAddress && uint64(rva) < uint64(s.VirtualAddress)+uint64(s.Size) {
			b, e := s.Data()
			if e != nil {
				return e
			}
			data = b[rva-s.VirtualAddress:]
			break
		}
	}
	if rva == 0 || len(data) == 0 {
		return fmt.Errorf("missing PE resource directory")
	}
	// Walk numeric type/name/language entries; all offsets are resource-relative.
	resource := func(typ uint32) ([]byte, error) {
		off := uint32(0)
		for depth, id := range []uint32{typ, 1, 0} {
			if uint64(off)+16 > uint64(len(data)) {
				return nil, fmt.Errorf("truncated directory")
			}
			count := uint32(binary.LittleEndian.Uint16(data[off+12:])) + uint32(binary.LittleEndian.Uint16(data[off+14:]))
			found := false
			for i := uint32(0); i < count; i++ {
				p := uint64(off) + 16 + uint64(i)*8
				if p+8 > uint64(len(data)) {
					return nil, fmt.Errorf("truncated entry")
				}
				key := binary.LittleEndian.Uint32(data[p:])
				value := binary.LittleEndian.Uint32(data[p+4:])
				if depth == 2 || key == id {
					if (depth < 2) != (value&0x80000000 != 0) {
						return nil, fmt.Errorf("invalid resource tree")
					}
					off = value & 0x7fffffff
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("resource type %d missing", typ)
			}
		}
		if uint64(off)+16 > uint64(len(data)) {
			return nil, fmt.Errorf("truncated resource data entry")
		}
		start := binary.LittleEndian.Uint32(data[off:])
		size := binary.LittleEndian.Uint32(data[off+4:])
		if start < rva || uint64(start-rva)+uint64(size) > uint64(len(data)) {
			return nil, fmt.Errorf("resource data out of range")
		}
		return data[start-rva : start-rva+size], nil
	}
	ver, err := resource(16)
	if err != nil {
		return err
	}
	// Fixed file/product versions must both equal X.Y.Z.0.
	fixed := bytes.Index(ver, []byte{0xbd, 0x04, 0xef, 0xfe})
	if fixed < 0 || fixed+24 > len(ver) {
		return fmt.Errorf("missing VS_FIXEDFILEINFO")
	}
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return fmt.Errorf("invalid version")
	}
	nums := make([]uint32, 3)
	for i, p := range parts {
		n, e := strconv.ParseUint(p, 10, 16)
		if e != nil {
			return e
		}
		nums[i] = uint32(n)
	}
	for i, want := range []uint32{nums[0]<<16 | nums[1], nums[2] << 16, nums[0]<<16 | nums[1], nums[2] << 16} {
		if binary.LittleEndian.Uint32(ver[fixed+8+4*i:]) != want {
			return fmt.Errorf("fixed version does not match %s", version)
		}
	}
	for k, v := range map[string]string{
		"ProductName": "SoluTerminal", "FileDescription": "SoluTerminal background service",
		"CompanyName": "The SoluTerminal Authors", "LegalCopyright": "Apache-2.0",
		"OriginalFilename": "solud.exe", "FileVersion": version, "ProductVersion": version,
	} {
		key := wide(k)
		p := bytes.Index(ver, key)
		if p < 0 {
			return fmt.Errorf("missing version key %s", k)
		}
		p += len(key)
		p = (p + 3) &^ 3
		if !bytes.HasPrefix(ver[p:], wide(v)) {
			return fmt.Errorf("wrong value for %s", k)
		}
	}
	manifest, err := resource(24)
	if err != nil {
		return err
	}
	var doc struct {
		Trust struct {
			Security struct {
				Privileges struct {
					Level struct {
						Level string `xml:"level,attr"`
						UI    string `xml:"uiAccess,attr"`
					} `xml:"requestedExecutionLevel"`
				} `xml:"requestedPrivileges"`
			} `xml:"security"`
		} `xml:"trustInfo"`
	}
	if err := xml.Unmarshal(manifest, &doc); err != nil {
		return err
	}
	if doc.Trust.Security.Privileges.Level.Level != "asInvoker" || doc.Trust.Security.Privileges.Level.UI != "false" {
		return fmt.Errorf("manifest must use asInvoker, uiAccess=false")
	}
	return nil
}
