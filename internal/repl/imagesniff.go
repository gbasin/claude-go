package repl

import "os"

// imageMagicOK reports whether the file at p starts with a known image
// signature — delegates to isImageMagic (term.go), the single sniff table.
func imageMagicOK(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	var buf [64]byte
	n, _ := f.Read(buf[:])
	return isImageMagic(buf[:n])
}
