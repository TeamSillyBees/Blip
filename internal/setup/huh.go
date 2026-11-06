package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"charm.land/huh/v2"
	"github.com/charmbracelet/x/term"
)

type huhPrompter struct {
	input      io.Reader
	output     io.Writer
	accessible bool
}

func newHuhPrompter(input io.Reader, output io.Writer, accessible bool) *huhPrompter {
	// Match Huh's TERM=dumb fallback before running a form, so implicit
	// accessibility also uses our description, EOF and cancellation handling.
	return &huhPrompter{input: input, output: output, accessible: accessible || os.Getenv("TERM") == "dumb"}
}

func (p *huhPrompter) Select(ctx context.Context, title, description string, choices []Choice, initial string) (string, error) {
	value := initial
	options := make([]huh.Option[string], 0, len(choices))
	for _, choice := range choices {
		options = append(options, huh.NewOption(choice.Label, choice.Value))
	}
	field := huh.NewSelect[string]().Title(title).Description(description).Options(options...).Value(&value)
	err := p.run(ctx, field, description)
	if err != nil {
		return "", err
	}
	return value, err
}

func (p *huhPrompter) Input(ctx context.Context, spec TextInput) (string, error) {
	value := spec.Initial
	field := huh.NewInput().Title(spec.Title).Description(spec.Description).Value(&value).Password(spec.Secret)
	if spec.Validate != nil {
		field.Validate(spec.Validate)
	}
	err := p.run(ctx, field, spec.Description)
	if err != nil {
		return "", err
	}
	if err == nil && spec.Validate != nil {
		err = spec.Validate(value)
	}
	return value, err
}

func (p *huhPrompter) Confirm(ctx context.Context, title, description string, initial bool) (bool, error) {
	value := initial
	field := huh.NewConfirm().Title(title).Description(description).Affirmative("是").Negative("否").Value(&value)
	err := p.run(ctx, field, description)
	if err != nil {
		return false, err
	}
	return value, err
}

func (p *huhPrompter) run(ctx context.Context, field huh.Field, description string) (err error) {
	if err = ctx.Err(); err != nil {
		return err
	}
	if p.accessible {
		// Huh v2.0.3 Form.runAccessible ignores field errors. Run its accessible
		// field directly so a failed password read can never become a blank value.
		if description != "" {
			fmt.Fprintln(p.output, description)
		}
		// Password reads use the descriptor directly. Restore console settings
		// when cancellation releases the caller before a blocked read finishes.
		if file, ok := p.input.(interface{ Fd() uintptr }); ok {
			if state, stateErr := term.GetState(file.Fd()); stateErr == nil {
				defer term.Restore(file.Fd(), state) //nolint:errcheck
			}
		}
		done := make(chan error, 1)
		go func() {
			done <- runAccessibleField(field, &observedReader{Reader: p.input, ctx: ctx}, &contextWriter{Writer: p.output, ctx: ctx})
		}()
		select {
		case err = <-done:
			fmt.Fprintln(p.output)
		case <-ctx.Done():
			return ctx.Err()
		}
	} else {
		err = huh.NewForm(huh.NewGroup(field)).WithAccessible(false).WithInput(p.input).WithOutput(p.output).RunWithContext(ctx)
	}
	if errors.Is(err, huh.ErrUserAborted) {
		return ErrCancelled
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// Keep field-local scanners from consuming input intended for the next field,
// retain the terminal descriptor for Huh's masked accessible password reader,
// and make EOF cancellation observable even when Huh returns a default value.
type observedReader struct {
	io.Reader
	err error
	ctx context.Context
}

func (r *observedReader) Read(buffer []byte) (int, error) {
	if r.ctx.Err() != nil {
		r.err = r.ctx.Err()
		return 0, io.EOF
	}
	if len(buffer) > 1 {
		buffer = buffer[:1]
	}
	n, err := r.Reader.Read(buffer)
	if err != nil {
		r.err = err
	}
	if n > 0 && buffer[0] == 3 {
		r.err = ErrCancelled
		return 0, io.EOF
	}
	return n, err
}

func (r *observedReader) Fd() uintptr { return r.Reader.(interface{ Fd() uintptr }).Fd() }

func runAccessibleField(field huh.Field, reader *observedReader, output io.Writer) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			// Huh's accessible Select may index an invalid result after EOF.
			if reader.err != nil {
				err = ErrCancelled
				return
			}
			panic(recovered)
		}
	}()
	field.Init()
	field.Focus()
	err = field.RunAccessible(output, reader)
	if reader.err != nil {
		return ErrCancelled
	}
	return err
}

type contextWriter struct {
	io.Writer
	ctx context.Context
}

func (w *contextWriter) Write(value []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.Writer.Write(value)
}

func isTerminal(value any) bool {
	file, ok := value.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(file.Fd())
}
