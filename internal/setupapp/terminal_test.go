package setupapp

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
)

func multiChoiceQuestion() Question {
	return Question{
		ID:     "agents",
		Prompt: "Which agents should this project realize?",
		Kind:   QuestionMultipleChoice,
		Options: []Option{
			{Value: "claude-code", Label: "Claude Code"},
			{Value: "codex", Label: "Codex", Selected: true},
			{Value: "cursor", Label: "Cursor"},
		},
	}
}

func rollbackQuestion() Question {
	return Question{
		ID:     "downgrade",
		Prompt: "This install rolls the dependency back.",
		Kind:   QuestionSingleChoice,
		Cancel: "cancel",
		Options: []Option{
			{Value: "hold", Label: "Hold latest behind a resume barrier"},
			{Value: "pin", Label: "Replace latest with a permanent pin"},
			{Value: "cancel", Label: "Cancel"},
		},
	}
}

func TestTerminalPrompterReadsInjectedStreams(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		question Question
		input    string
		want     Answer
	}{
		{name: "numbers", question: multiChoiceQuestion(), input: "1,3\n", want: Answer{Values: []string{"claude-code", "cursor"}}},
		{name: "names", question: multiChoiceQuestion(), input: "cursor codex\n", want: Answer{Values: []string{"cursor", "codex"}}},
		{name: "repeated selection", question: multiChoiceQuestion(), input: "codex, codex\n", want: Answer{Values: []string{"codex"}}},
		{name: "default", question: multiChoiceQuestion(), input: "\n", want: Answer{Values: []string{"codex"}}},
		{name: "single choice", question: rollbackQuestion(), input: "hold\n", want: Answer{Values: []string{"hold"}}},
		{name: "explicit cancel", question: rollbackQuestion(), input: "3\n", want: Answer{Cancelled: true}},
		{name: "empty without a default", question: rollbackQuestion(), input: "\n", want: Answer{Cancelled: true}},
		{name: "end of input", question: rollbackQuestion(), input: "", want: Answer{Cancelled: true}},
		{name: "three unparsable answers", question: rollbackQuestion(), input: "no\nnope\nstill no\nhold\n", want: Answer{Cancelled: true}},
		{name: "two choices for one", question: rollbackQuestion(), input: "hold pin\nhold\n", want: Answer{Values: []string{"hold"}}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var written bytes.Buffer
			prompter := NewTerminalPrompter(strings.NewReader(test.input), &written, true)

			answer, err := prompter.Ask(context.Background(), test.question)

			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(answer, test.want) {
				t.Fatalf("Ask() = %#v, want %#v", answer, test.want)
			}
			if !strings.Contains(written.String(), test.question.Prompt) {
				t.Fatalf("question was not rendered: %q", written.String())
			}
			for _, option := range test.question.Options {
				if !strings.Contains(written.String(), option.Label) {
					t.Fatalf("option %q was not rendered: %q", option.Label, written.String())
				}
			}
		})
	}
}

func TestNonInteractivePrompterNeverReads(t *testing.T) {
	t.Parallel()

	reader := &countingReader{}
	var written bytes.Buffer
	prompter := NewTerminalPrompter(reader, &written, false)

	answer, err := prompter.Ask(context.Background(), rollbackQuestion())

	if err != nil || !answer.Cancelled || len(answer.Values) != 0 {
		t.Fatalf("Ask() = %#v, %v, want a cancelled answer", answer, err)
	}
	if reader.reads != 0 {
		t.Fatalf("non-interactive prompter read %d time(s), want 0", reader.reads)
	}
	if written.Len() != 0 {
		t.Fatalf("non-interactive prompter wrote %q, want nothing", written.String())
	}
}

// countingReader records whether a prompter touched the input stream at all.
// bufio reads eagerly, so any read means the prompter asked.
type countingReader struct {
	reads int
}

func (reader *countingReader) Read([]byte) (int, error) {
	reader.reads++
	return 0, nil
}
