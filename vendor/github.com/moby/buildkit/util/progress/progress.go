package progress

import (
	"context"
	"io"
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/pkg/errors"
)

// Progress package provides utility functions for using the context to capture
// progress of a running function. All progress items written contain an ID
// that is used to collapse unread messages.

type contextKeyT string

var contextKey = contextKeyT("buildkit/util/progress")

// WriterFactory will generate a new progress Writer and return a new Context
// with the new Writer stored.  It is the callers responsibility to Close the
// returned Writer to avoid resource leaks.
type WriterFactory func(ctx context.Context) (Writer, bool, context.Context)

// FromContext returns a WriterFactory to generate new progress writers based
// on a Writer previously stored in the Context.
func FromContext(ctx context.Context, opts ...WriterOption) WriterFactory {
	v := ctx.Value(contextKey)
	return func(ctx context.Context) (Writer, bool, context.Context) {
		pw, ok := v.(*progressWriter)
		if !ok {
			if pw, ok := v.(*MultiWriter); ok {
				return pw, true, ctx
			}
			return &noOpWriter{}, false, ctx
		}
		pw = newWriter(pw)
		for _, o := range opts {
			o(pw)
		}
		ctx = context.WithValue(ctx, contextKey, pw)
		return pw, true, ctx
	}
}

// NewFromContext creates a new Writer based on a Writer previously stored
// in the Context and returns a new Context with the new Writer stored.  It is
// the callers responsibility to Close the returned Writer to avoid resource leaks.
func NewFromContext(ctx context.Context, opts ...WriterOption) (Writer, bool, context.Context) {
	return FromContext(ctx, opts...)(ctx)
}

type WriterOption func(Writer)

// NewContext returns a new context and a progress reader that captures all
// progress items writtern to this context. Last returned parameter is a closer
// function to signal that no new writes will happen to this context.
func NewContext(ctx context.Context) (Reader, context.Context, func(error)) {
	pr, pw, cancel := pipe()
	ctx = WithProgress(ctx, pw)
	return pr, ctx, cancel
}

func WithProgress(ctx context.Context, pw Writer) context.Context {
	return context.WithValue(ctx, contextKey, pw)
}

func WithMetadata(key string, val any) WriterOption {
	return func(w Writer) {
		if pw, ok := w.(*progressWriter); ok {
			pw.meta[key] = val
		}
		if pw, ok := w.(*MultiWriter); ok {
			pw.meta[key] = val
		}
	}
}

type Controller interface {
	Start(context.Context) (context.Context, func(error))
	Status(id string, action string) func()
}

type Writer interface {
	Write(id string, value any) error
	Close() error
}

type Reader interface {
	Read(context.Context) ([]*Progress, error)
}

type Progress struct {
	ID        string
	Timestamp time.Time
	Sys       any
	meta      map[string]any
}

type Status struct {
	Action    string
	Current   int
	Total     int
	Started   *time.Time
	Completed *time.Time
}

type progressReader struct {
	ctx     context.Context
	cond    *sync.Cond
	mu      sync.Mutex
	writers map[*progressWriter]struct{}
	dirty   map[string]*Progress
}

func (pr *progressReader) Read(ctx context.Context) ([]*Progress, error) {
	done := make(chan struct{})
	defer close(done)
	go func() {
		prdone := pr.ctx.Done()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				pr.mu.Lock()
				pr.cond.Broadcast()
				pr.mu.Unlock()
				return
			case <-prdone:
				pr.mu.Lock()
				pr.cond.Broadcast()
				pr.mu.Unlock()
				prdone = nil
			}
		}
	}()
	pr.mu.Lock()
	for {
		select {
		case <-ctx.Done():
			pr.mu.Unlock()
			return nil, context.Cause(ctx)
		default:
		}
		dmap := pr.dirty
		if len(dmap) == 0 {
			select {
			case <-pr.ctx.Done():
				if len(pr.writers) == 0 {
					pr.mu.Unlock()
					return nil, io.EOF
				}
			default:
			}
			pr.cond.Wait()
			continue
		}
		pr.dirty = make(map[string]*Progress)
		pr.mu.Unlock()

		out := make([]*Progress, 0, len(dmap))
		for _, p := range dmap {
			out = append(out, p)
		}

		sort.Slice(out, func(i, j int) bool {
			return out[i].Timestamp.Before(out[j].Timestamp)
		})

		return out, nil
	}
}

func (pr *progressReader) append(pw *progressWriter) {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	select {
	case <-pr.ctx.Done():
		return
	default:
		pr.writers[pw] = struct{}{}
	}
}

func pipe() (*progressReader, *progressWriter, func(error)) {
	ctx, cancel := context.WithCancelCause(context.Background())
	pr := &progressReader{
		ctx:     ctx,
		writers: make(map[*progressWriter]struct{}),
		dirty:   make(map[string]*Progress),
	}
	pr.cond = sync.NewCond(&pr.mu)
	go func() {
		<-ctx.Done()
		pr.mu.Lock()
		pr.cond.Broadcast()
		pr.mu.Unlock()
	}()
	pw := &progressWriter{
		reader: pr,
	}
	return pr, pw, cancel
}

func newWriter(pw *progressWriter) *progressWriter {
	meta := make(map[string]any)
	maps.Copy(meta, pw.meta)
	pw = &progressWriter{
		reader: pw.reader,
		meta:   meta,
	}
	pw.reader.append(pw)
	return pw
}

type progressWriter struct {
	done   bool
	reader *progressReader
	meta   map[string]any
}

func (pw *progressWriter) Write(id string, v any) error {
	if pw.done {
		return errors.Errorf("writing %s to closed progress writer", id)
	}
	return pw.writeRawProgress(&Progress{
		ID:        id,
		Timestamp: time.Now(),
		Sys:       v,
		meta:      pw.meta,
	})
}

func (pw *progressWriter) WriteRawProgress(p *Progress) error {
	meta := p.meta
	if len(pw.meta) > 0 {
		meta = map[string]any{}
		maps.Copy(meta, p.meta)
		for k, v := range pw.meta {
			if _, ok := meta[k]; !ok {
				meta[k] = v
			}
		}
	}
	p.meta = meta
	return pw.writeRawProgress(p)
}

func (pw *progressWriter) writeRawProgress(p *Progress) error {
	pw.reader.mu.Lock()
	pw.reader.dirty[p.ID] = p
	pw.reader.cond.Broadcast()
	pw.reader.mu.Unlock()
	return nil
}

func (pw *progressWriter) Close() error {
	pw.reader.mu.Lock()
	delete(pw.reader.writers, pw)
	pw.reader.mu.Unlock()
	pw.reader.cond.Broadcast()
	pw.done = true
	return nil
}

func (p *Progress) Meta(key string) (any, bool) {
	v, ok := p.meta[key]
	return v, ok
}

type noOpWriter struct{}

func (pw *noOpWriter) Write(_ string, _ any) error {
	return nil
}

func (pw *noOpWriter) Close() error {
	return nil
}

func OneOff(ctx context.Context, id string) func(err error) error {
	pw, _, _ := NewFromContext(ctx)
	now := time.Now()
	st := Status{
		Started: &now,
	}
	pw.Write(id, st)
	return func(err error) error {
		// TODO: set error on status
		now := time.Now()
		st.Completed = &now
		pw.Write(id, st)
		pw.Close()
		return err
	}
}
