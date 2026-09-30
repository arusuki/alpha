// Package sudoutil handles a one-shot sudo password without retaining diagnostics.
package sudoutil

import (
	"io"
	"sync"
)

// NewInput takes ownership of password, sends it only in response to prompt,
// and wipes it after the first send. Clear must also be called on every exit.
func NewInput(input io.WriteCloser, password []byte, prompt string) *Input {
	if prompt == "" {
		panic("empty sudo prompt")
	}
	return &Input{input: input, password: password, prompt: prompt}
}

// Never retain sudo output: PAM/plug-ins may echo input in their diagnostics.
// Only the exact prompt is recognized; send one password and wipe it immediately.
// Waiting for a prompt also keeps credentials out of the helper's stdin when
// sudo requires no authentication (root or a NOPASSWD rule).
type Input struct {
	mu          sync.Mutex
	prompt      string
	input       io.WriteCloser
	password    []byte
	matched     int
	sent, ready bool
}

func (p *Input) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range data {
		if b == p.prompt[p.matched] {
			p.matched++
		} else {
			p.matched = 0
			if b == p.prompt[0] {
				p.matched = 1
			}
		}
		if p.matched != len(p.prompt) {
			continue
		}
		p.matched = 0
		if p.sent || p.ready {
			_ = p.input.Close()
			continue
		}
		p.sent = true
		_, err := p.input.Write(p.password)
		clear(p.password)
		p.password = nil
		if err == nil {
			_, err = p.input.Write([]byte{'\n'})
		}
		if err != nil {
			_ = p.input.Close()
		}
	}
	return len(data), nil
}
func (p *Input) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.password)
	p.password = nil
	p.ready = true
}
