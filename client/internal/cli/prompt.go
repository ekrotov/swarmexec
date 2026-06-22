package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
)

// A single shared reader over os.Stdin. Using one reader avoids the classic bug
// where two separate bufio scanners over the same fd let the first buffer past
// its line and swallow input meant for the next prompt (e.g. `down` asks for a
// context choice and then a confirmation).
var (
	stdinOnce sync.Once
	stdinBuf  *bufio.Reader
)

func stdinReader() *bufio.Reader {
	stdinOnce.Do(func() { stdinBuf = bufio.NewReader(os.Stdin) })
	return stdinBuf
}

// promptLine writes prompt (if any) to stderr and reads one line from stdin,
// returning it trimmed of the trailing newline. ok is false on EOF with no data.
func promptLine(prompt string) (line string, ok bool) {
	if prompt != "" {
		fmt.Fprint(os.Stderr, prompt)
	}
	s, err := stdinReader().ReadString('\n')
	s = strings.TrimRight(s, "\r\n")
	if err != nil && s == "" {
		return "", false
	}
	return s, true
}
