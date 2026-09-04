package setupapp

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestPromptHintMatchesQuestionArity covers issue #82: the agent question
// accepts a comma-separated list but advertised one answer, and the freshness
// question accepts one policy but advertised several. Arity comes from the
// question's kind; the Enter shortcut comes from its defaults.
func TestPromptHintMatchesQuestionArity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		question Question
		want     string
	}{
		{
			name:     "multiple choice without a default",
			question: hintQuestion(QuestionMultipleChoice, false),
			want:     "Answer with numbers or names: ",
		},
		{
			name:     "multiple choice with a default",
			question: hintQuestion(QuestionMultipleChoice, true),
			want:     "Answer with numbers or names, or press Enter to accept the selection: ",
		},
		{
			name:     "single choice without a default",
			question: hintQuestion(QuestionSingleChoice, false),
			want:     "Answer with a number or a name: ",
		},
		{
			name:     "single choice with a default",
			question: hintQuestion(QuestionSingleChoice, true),
			want:     "Answer with a number or a name, or press Enter to accept the selection: ",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var rendered bytes.Buffer
			prompter := NewTerminalPrompter(strings.NewReader("1\n"), &rendered, true)
			if _, err := prompter.Ask(context.Background(), test.question); err != nil {
				t.Fatalf("Ask() error = %v", err)
			}
			if !strings.HasSuffix(rendered.String(), test.want) {
				t.Errorf("rendered question = %q, want it to end with %q", rendered.String(), test.want)
			}
		})
	}
}

// TestInitQuestionHintsMatchWhatTheyAccept pins the two questions the dogfood
// session actually saw.
func TestInitQuestionHintsMatchWhatTheyAccept(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		question Question
		answer   string
		want     string
		values   int
	}{
		{name: "agents", question: agentQuestion(nil, nil), answer: "1,2\n", want: "Answer with numbers or names: ", values: 2},
		{name: "freshness", question: freshnessQuestion(), answer: "1\n", want: "Answer with a number or a name, or press Enter to accept the selection: ", values: 1},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var rendered bytes.Buffer
			prompter := NewTerminalPrompter(strings.NewReader(test.answer), &rendered, true)
			answer, err := prompter.Ask(context.Background(), test.question)
			if err != nil {
				t.Fatalf("Ask() error = %v", err)
			}
			if !strings.HasSuffix(rendered.String(), test.want) {
				t.Errorf("rendered question = %q, want it to end with %q", rendered.String(), test.want)
			}
			if len(answer.Values) != test.values {
				t.Errorf("answer values = %v, want %d of them", answer.Values, test.values)
			}
		})
	}
}

func hintQuestion(kind QuestionKind, withDefault bool) Question {
	return Question{
		ID:     "hint",
		Prompt: "Which option?",
		Kind:   kind,
		Options: []Option{
			{Value: "first", Label: "First", Selected: withDefault},
			{Value: "second", Label: "Second"},
		},
	}
}
