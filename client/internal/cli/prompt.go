// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	cterm "swarmexec/client/internal/term"
)

// A single shared reader over os.Stdin. Using one reader avoids the classic bug
// where two separate bufio scanners over the same fd let the first buffer past
// its line and swallow input meant for the next prompt (e.g. `down` asks for a
// context choice and then a confirmation).
var (
	stdinOnce sync.Once
	stdinBuf  *bufio.Reader
)

// promptSeam is where prompts read their answers and write their questions.
// The zero value is the process: os.Stdin, os.Stderr, and "interactive" meaning
// stdin is a terminal. Tests set it (usePrompts) to play the operator.
type promptSeam struct {
	in          *bufio.Reader // nil: the shared reader over os.Stdin
	out         io.Writer     // nil: os.Stderr
	interactive func() bool   // nil: stdin is a terminal
}

var prompts promptSeam

func (p promptSeam) reader() *bufio.Reader {
	if p.in != nil {
		return p.in
	}
	stdinOnce.Do(func() { stdinBuf = bufio.NewReader(os.Stdin) })
	return stdinBuf
}

func (p promptSeam) writer() io.Writer {
	if p.out != nil {
		return p.out
	}
	return os.Stderr
}

// stdinIsTerminal reports whether prompts may ask: stdin is a terminal.
func stdinIsTerminal() bool {
	if prompts.interactive != nil {
		return prompts.interactive()
	}
	return cterm.IsTerminal(os.Stdin.Fd())
}

// promptLine writes prompt (if any) to stderr and reads one line from stdin,
// returning it trimmed of the trailing newline. ok is false on EOF with no data.
func promptLine(prompt string) (line string, ok bool) {
	if prompt != "" {
		fmt.Fprint(prompts.writer(), prompt)
	}
	s, err := prompts.reader().ReadString('\n')
	s = strings.TrimRight(s, "\r\n")
	if err != nil && s == "" {
		return "", false
	}
	return s, true
}
