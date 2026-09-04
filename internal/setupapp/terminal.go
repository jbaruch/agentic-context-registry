package setupapp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// maxPromptAttempts bounds how many unparsable answers one question tolerates
// before it is cancelled, so a stream of noise cannot loop forever.
const maxPromptAttempts = 3

// TerminalPrompter reads answers from an injected reader and writes questions
// to an injected writer. It never touches os.Stdin, os.Stderr, or /dev/tty:
// cmd/acr owns the one character-device probe and passes its result in, so a
// test exercises every path without a terminal.
type TerminalPrompter struct {
	reader      *bufio.Reader
	writer      io.Writer
	interactive bool
}

// NewTerminalPrompter constructs a prompter over the supplied streams.
func NewTerminalPrompter(reader io.Reader, writer io.Writer, interactive bool) *TerminalPrompter {
	return &TerminalPrompter{reader: bufio.NewReader(reader), writer: writer, interactive: interactive}
}

// Interactive reports whether this prompter may ask anything at all.
func (prompter *TerminalPrompter) Interactive() bool {
	return prompter.interactive
}

// Ask writes one question to the diagnostic stream and reads one answer. A
// non-interactive prompter never reads: it cancels so the caller returns its
// typed refusal instead.
//
// End of input cancels before the line is parsed, whatever the line holds and
// whatever the question offers. A stream that ends mid-question is an operator
// who is not there, so a preselected default must not stand in for an answer
// nobody gave, and a partial line without its newline is not a submitted one.
// Cancelling there is also what keeps every path from blocking.
func (prompter *TerminalPrompter) Ask(ctx context.Context, question Question) (Answer, error) {
	if !prompter.interactive {
		return Answer{Cancelled: true}, nil
	}
	for attempt := 0; attempt < maxPromptAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return Answer{}, err
		}
		if err := prompter.render(question); err != nil {
			return Answer{}, err
		}
		line, readErr := prompter.reader.ReadString('\n')
		if errors.Is(readErr, io.EOF) {
			return Answer{Cancelled: true}, nil
		}
		if readErr != nil {
			return Answer{}, fmt.Errorf("read the answer to %q: %w; rerun with --non-interactive and pass the selection as flags", question.ID, readErr)
		}
		answer, parsed := question.parse(line)
		if parsed {
			return answer, nil
		}
		if err := prompter.write("That is not one of the options.\n"); err != nil {
			return Answer{}, err
		}
	}
	return Answer{Cancelled: true}, nil
}

func (prompter *TerminalPrompter) render(question Question) error {
	var builder strings.Builder
	builder.WriteString(question.Prompt)
	builder.WriteByte('\n')
	for index, option := range question.Options {
		builder.WriteString("  " + positionLabel(index) + ") " + option.Label)
		if option.Selected {
			builder.WriteString(" [selected]")
		}
		builder.WriteByte('\n')
	}
	// Arity comes from the question's kind and the Enter shortcut from its
	// defaults. They are independent: a single-choice question can carry a
	// preselected option, and a multiple-choice question can carry none.
	if question.Kind == QuestionMultipleChoice {
		builder.WriteString("Answer with numbers or names")
	} else {
		builder.WriteString("Answer with a number or a name")
	}
	if len(question.defaults()) != 0 {
		builder.WriteString(", or press Enter to accept the selection")
	}
	builder.WriteString(": ")
	return prompter.write(builder.String())
}

func (prompter *TerminalPrompter) write(text string) error {
	if _, err := io.WriteString(prompter.writer, text); err != nil {
		return fmt.Errorf("write the setup question: %w; verify stderr is writable and retry", err)
	}
	return nil
}

func positionLabel(index int) string {
	return strconv.Itoa(index + 1)
}
