// mockd runs the mock Anthropic API standalone on a fixed address —
// the capture/relay harness targets it so real claude can complete
// turns against it while requests are recorded for golden diffs.
package main

import (
	"claude-go/internal/mock"
	"fmt"
	"os"
)

func main() {
	addr := "127.0.0.1:8898"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}
	s, err := mock.StartOn(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("mock on", s.Addr)
	select {}
}
