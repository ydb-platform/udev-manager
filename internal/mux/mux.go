package mux

import (
	"fmt"
	"time"
)

type In[T any] <-chan T
type Out[T any] chan<- T

type Logger interface {
	Info(format string, args ...interface{})
}

type AwaitReply[T, U any] struct {
	value T
	reply chan U
}

func (ar AwaitReply[T, U]) Value() T {
	return ar.value
}

func (ar AwaitReply[T, U]) Reply(value U) {
	ar.reply <- value
	close(ar.reply)
}

func (ar AwaitReply[T, U]) Await() U {
	return <-ar.reply
}

type AwaitDone[T any] struct {
	AwaitReply[T, struct{}]
}

func (ad AwaitDone[T]) Done() {
	ad.Reply(struct{}{})
}

func (ad AwaitDone[T]) Wait() {
	ad.Await()
}

func NewAwaitReply[T, U any](value T) AwaitReply[T, U] {
	return AwaitReply[T, U]{
		value: value,
		reply: make(chan U),
	}
}

func NewAwaitDone[T any](value T) AwaitDone[T] {
	return AwaitDone[T]{
		NewAwaitReply[T, struct{}](value),
	}
}

type Sink[T any] interface {
	Submit(T) error
	Close()
}

type thenSink[U, T any] struct {
	sink Sink[T]
	contramap func(U) T
}

func (c *thenSink[U, T]) Submit(v U) error {
	return c.sink.Submit(c.contramap(v))
}

func (c *thenSink[U, T]) Close() {
	c.sink.Close()
}

func ThenSink[U, T any](sink Sink[T], f func(U) T) Sink[U] {
	return &thenSink[U, T]{sink, f}
}

type filterSink[T any] struct {
	sink Sink[T]
	f func(T) bool
}

func (c *filterSink[T]) Submit(v T) error {
	if c.f(v) {
		return c.sink.Submit(v)
	}
	return nil
}

func (c *filterSink[T]) Close() {
	c.sink.Close()
}

func FilterSink[T any](sink Sink[T], f func(T) bool) Sink[T] {
	return &filterSink[T]{sink, f}
}

type chanSink[T any] struct {
	ch chan<- T
}

func (c *chanSink[T]) Submit(v T) error {
	c.ch <- v
	return nil
}

func (c *chanSink[T]) Close() {
	close(c.ch)
}

func SinkFromChan[T any](ch chan<- T) Sink[T] {
	return &chanSink[T]{ch}
}

type Source[T any] interface {
	Subscribe(Sink[T]) CancelFunc
}

type Mux[T any] struct {
	input chan T
	register chan AwaitDone[Sink[T]]
	unregister chan AwaitDone[Sink[T]]
	outputs map[Sink[T]]bool

	submitTimeout time.Duration
	inBufSize int
	logger Logger
}

type Option[T any] interface {
	apply(*Mux[T])
}

type buffered[T any] struct {
	Size int
}

func (b *buffered[T]) apply(m *Mux[T]) {
	m.inBufSize = b.Size
}

func Buffered[T any](size int) Option[T] {
	return &buffered[T]{size}
}

type withLogger[T any] struct {
	Logger Logger
}

func (l *withLogger[T]) apply(m *Mux[T]) {
	m.logger = l.Logger
}

func WithLogger[T any](logger Logger) Option[T] {
	return &withLogger[T]{logger}
}

func Make[T any](opts ...Option[T]) *Mux[T] {
	mux := &Mux[T]{
		submitTimeout: 1 * time.Second,
	}

	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt.apply(mux)
	}

	mux.input = make(chan T, mux.inBufSize)
	mux.register = make(chan AwaitDone[Sink[T]])
	mux.unregister = make(chan AwaitDone[Sink[T]])
	mux.outputs = make(map[Sink[T]]bool)

	go mux.run()

	return mux
}

func (c *Mux[T]) run() {
	defer func() {
		for sub := range c.outputs {
			delete(c.outputs, sub)
			sub.Close()
		}
	}()
	defer close(c.input)
	
	for {
		select {
		case v := <-c.input:
			for out := range c.outputs {
				if err := out.Submit(v); err != nil {
					c.error("error submitting value %v: %v", v, err)
				}
			}
		case ar, ok := <-c.register:
			if !ok {
				return
			}
			c.outputs[ar.value] = true
			ar.reply <- struct{}{}
		case ar := <-c.unregister:
			sub := ar.value
			delete(c.outputs, sub)
			sub.Close()
			ar.reply <- struct{}{}
		}
	}
}

func (m *Mux[T]) error(format string, args ...any) error {
	if m.logger != nil {
		m.logger.Info(format, args...)
	}
	return fmt.Errorf(format, args...)
}

func (c *Mux[T]) Close() {
	close(c.register)
}


func (c *Mux[T]) Submit(v T) error {
	select {
	case c.input <- v:
		return nil
	case <-time.After(c.submitTimeout):
		return c.error("timed out submitting value %v after %s", v, c.submitTimeout)
	}
}

type CancelFunc func()

func (c *Mux[T]) Subscribe(sink Sink[T]) CancelFunc {
	ar := NewAwaitDone(sink)
	c.register <- ar
	ar.Wait()

	return func() {
		ar := NewAwaitDone(sink)
		c.unregister <- ar
		ar.Wait()
	}
}

func ChainCancelFunc(cf1, cf2 func(), cfs ...func()) CancelFunc {
	return func() {
		cf1()
		cf2()
		for _, cf := range cfs {
			if cf != nil {
				cf()
			}
		}
	}
}
