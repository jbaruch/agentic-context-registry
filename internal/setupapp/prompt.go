// Package setupapp is the outermost cli.Application decorator and the only
// place in acr that holds a Prompter. Every application beneath it — publish,
// freshness, realization, dependency — stays prompt-free, and the domain work
// the questions feed lives in internal/setup, which has no prompter at all.
package setupapp

import (
	"context"
	"strings"
	"unicode"
)

// QuestionKind selects how many options one answer may name.
type QuestionKind string

const (
	// QuestionSingleChoice accepts exactly one option.
	QuestionSingleChoice QuestionKind = "single-choice"
	// QuestionMultipleChoice accepts one or more options.
	QuestionMultipleChoice QuestionKind = "multiple-choice"
)

// Option is one selectable answer. Selected marks what a bare newline accepts;
// a question with no selected option has no default, so an empty answer
// cancels it rather than guessing.
type Option struct {
	Value    string
	Label    string
	Selected bool
}

// Question is one prompt. Cancel names the option value that means the
// operator declined; leave it empty when a question cannot be declined.
type Question struct {
	ID      string
	Prompt  string
	Kind    QuestionKind
	Cancel  string
	Options []Option
}

// Answer is a parsed response. Cancelled reports an explicit cancel, an empty
// answer to a question with no default, end of input, or answers the prompter
// could not parse and stopped re-asking.
type Answer struct {
	Values    []string
	Cancelled bool
}

// Prompter asks the operator one question. Interactive is fixed when the
// prompter is constructed: no implementation probes a terminal, so a test
// injects both the mode and the answers.
type Prompter interface {
	Interactive() bool
	Ask(ctx context.Context, question Question) (Answer, error)
}

// defaults returns the option values a bare newline accepts.
func (question Question) defaults() []string {
	var values []string
	for _, option := range question.Options {
		if option.Selected {
			values = append(values, option.Value)
		}
	}
	return values
}

// parse interprets one answer line, reporting false when the line names
// something the question does not offer.
func (question Question) parse(line string) (Answer, bool) {
	fields := strings.FieldsFunc(line, func(character rune) bool {
		return character == ',' || unicode.IsSpace(character)
	})
	if len(fields) == 0 {
		defaults := question.defaults()
		if len(defaults) == 0 {
			return Answer{Cancelled: true}, true
		}
		return Answer{Values: defaults}, true
	}
	if question.Kind == QuestionSingleChoice && len(fields) != 1 {
		return Answer{}, false
	}
	values := make([]string, 0, len(fields))
	for _, field := range fields {
		value, known := question.resolve(field)
		if !known {
			return Answer{}, false
		}
		if value == question.Cancel {
			return Answer{Cancelled: true}, true
		}
		if !contains(values, value) {
			values = append(values, value)
		}
	}
	return Answer{Values: values}, true
}

// resolve accepts either an option's one-based position or its value.
func (question Question) resolve(field string) (string, bool) {
	for index, option := range question.Options {
		if field == positionLabel(index) || strings.EqualFold(field, option.Value) {
			return option.Value, true
		}
	}
	return "", false
}

func contains(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}
