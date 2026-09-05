package setupapp

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestFFAPromptHints(t *testing.T) {
	for _, kind := range []QuestionKind{QuestionSingleChoice, QuestionMultipleChoice} {
		for _, selected := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/default=%t", kind, selected), func(t *testing.T) {
				question := Question{ID: "ffa", Prompt: "Choose", Kind: kind, Options: []Option{{Value: "alpha", Label: "alpha", Selected: selected}, {Value: "beta", Label: "beta"}}}
				input := "1\n"
				want := []string{"alpha"}
				if kind == QuestionMultipleChoice {
					input = "1,2\n"
					want = []string{"alpha", "beta"}
				}
				var output bytes.Buffer
				answer, err := NewTerminalPrompter(strings.NewReader(input), &output, true).Ask(context.Background(), question)
				if err != nil || answer.Cancelled || !reflect.DeepEqual(answer.Values, want) {
					t.Fatalf("answer=%#v err=%v", answer, err)
				}
				hint := strings.ToLower(output.String())
				plural := strings.Contains(hint, "numbers") && strings.Contains(hint, "names")
				if plural != (kind == QuestionMultipleChoice) {
					t.Errorf("hint arity does not match %s: %q", kind, hint)
				}
				if strings.Contains(hint, "enter") != selected {
					t.Errorf("default hint mismatch: %q", hint)
				}
				output.Reset()
				blank, err := NewTerminalPrompter(strings.NewReader("\n"), &output, true).Ask(context.Background(), question)
				if err != nil {
					t.Fatal(err)
				}
				if selected {
					if blank.Cancelled || !reflect.DeepEqual(blank.Values, []string{"alpha"}) {
						t.Errorf("blank failed default: %#v", blank)
					}
				} else if !blank.Cancelled || len(blank.Values) != 0 {
					t.Errorf("blank without default guessed: %#v", blank)
				}
			})
		}
	}
}

func TestFFAPromptInvalidAnswers(t *testing.T) {
	question := Question{ID: "policy", Kind: QuestionSingleChoice, Options: []Option{{Value: "alpha", Label: "alpha", Selected: true}, {Value: "beta", Label: "beta"}}}
	for _, input := range []string{"1,2\n1,2\n1,2\n", "0\n0\n0\n", "3\n3\n3\n", "unknown\nunknown\nunknown\n", ""} {
		var output bytes.Buffer
		answer, err := NewTerminalPrompter(strings.NewReader(input), &output, true).Ask(context.Background(), question)
		if err != nil || !answer.Cancelled || len(answer.Values) != 0 {
			t.Errorf("invalid %q answer=%#v err=%v", input, answer, err)
		}
	}
}
