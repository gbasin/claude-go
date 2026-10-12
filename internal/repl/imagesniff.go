package repl

// Image magic sniff for the dragged-file-path paste → "[Image #N]"
// flow — audits/tui.md §62/§91. Real's paste path (mod_0263 R()) sniffs
// the dropped file's signature, so a path whose extension lies (or is
// missing) is classified by content.
//
// HOOK (editor-agent): term.go's sniffImagePath (term.go:~3277) gates
// on extension + os.Stat only. It should call imageMagicOK(p) after the
// stat check — returning "" when the file's magic isn't a known image
// (keeps a renamed .txt-as-.png out of the image lane), and optionally
// accepting a path with real image magic even when the extension isn't
// in the list.

import (
	"bytes"
	"os"
)

// imageMagicOK reports whether the file at p starts with a known image
// signature (png, jpeg, gif, webp, bmp, tiff, heic).
func imageMagicOK(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	var buf [12]byte
	n, err := f.Read(buf[:])
	if err != nil && n == 0 {
		return false
	}
	h := buf[:n]
	switch {
	case len(h) >= 8 && bytes.Equal(h[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}):
		return true
	case len(h) >= 3 && h[0] == 0xff && h[1] == 0xd8 && h[2] == 0xff:
		return true // jpeg SOI
	case len(h) >= 6 && (string(h[:6]) == "GIF87a" || string(h[:6]) == "GIF89a"):
		return true
	case len(h) >= 12 && string(h[:4]) == "RIFF" && string(h[8:12]) == "WEBP":
		return true
	case len(h) >= 2 && h[0] == 'B' && h[1] == 'M':
		return true // bmp
	case len(h) >= 4 && (bytes.Equal(h[:4], []byte{'I', 'I', 0x2a, 0}) ||
		bytes.Equal(h[:4], []byte{'M', 'M', 0, 0x2a})):
		return true // tiff LE/BE
	case len(h) >= 12 && string(h[4:8]) == "ftyp":
		switch string(h[8:12]) {
		case "heic", "heix", "hevc", "hevx", "heim", "heis", "mif1", "msf1":
			return true // heic/heif family
		}
	}
	return false
}
