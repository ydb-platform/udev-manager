package mux

import (
	"reflect"
)

// FilterFunc is a predicate over values of type T. It returns true if the value
// should be kept (passed downstream) and false if it should be dropped.
type FilterFunc[T any] func(T) bool

// MapperFunc is a transformation function that converts a value of type T to
// type U. It is used with combinator constructors such as [ThenSink] to adapt
// an event stream from one type to another.
type MapperFunc[T, U any] func(T) U

// Filter reads values from in and forwards only those for which filter returns
// true. Values that do not satisfy filter are silently dropped.
//
// The returned [In] channel is closed automatically when in is closed.
// Filter spawns a goroutine to drive the pipeline; the goroutine exits when
// the input channel closes.
func Filter[T any](in In[T], filter FilterFunc[T]) In[T] {
	out := make(chan T)

	go func() {
		defer close(out)

		for v := range in {
			if filter(v) {
				out <- v
			}
		}
	}()

	return out
}

// Any returns a [FilterFunc] that accepts every value, regardless of its
// content. It is useful as a default or no-op filter.
func Any[T any]() FilterFunc[T] {
	return func(T) bool {
		return true
	}
}

// Not returns a [FilterFunc] that inverts filter: it accepts values that filter
// rejects, and rejects values that filter accepts.
func Not[T any](filter FilterFunc[T]) FilterFunc[T] {
	return func(v T) bool {
		return !filter(v)
	}
}

// Or returns a [FilterFunc] that accepts a value if at least one of the
// provided filters accepts it (short-circuit OR). If no filters are provided,
// the result rejects all values.
func Or[T any](filters ...FilterFunc[T]) FilterFunc[T] {
	return func(v T) bool {
		for _, filter := range filters {
			if filter(v) {
				return true
			}
		}
		return false
	}
}

// And returns a [FilterFunc] that accepts a value only if all of the provided
// filters accept it (short-circuit AND). If no filters are provided, the result
// accepts all values.
func And[T any](filters ...FilterFunc[T]) FilterFunc[T] {
	return func(v T) bool {
		for _, filter := range filters {
			if !filter(v) {
				return false
			}
		}
		return true
	}
}

// IsNil reports whether v is nil. It handles all nilable Go kinds — interface,
// channel, func, map, pointer, unsafe pointer, and slice — using reflection.
// For non-nilable kinds (e.g. int, struct) it always returns false.
//
// IsNil is intended for use as a [FilterFunc] or as a building block for
// [Not](IsNil) (i.e. [IsNotNil]).
func IsNil[T any](v T) bool {
	rv := reflect.ValueOf(v)
	kind := rv.Kind()
	// Check if the type can be nil and is nil
	switch kind {
	case reflect.Invalid:
		return true
	case reflect.Interface:
		return !rv.IsValid()
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Ptr, reflect.UnsafePointer, reflect.Slice:
		return rv.IsNil()
	}

	return false
}

// IsNotNil reports whether v is not nil. It is the logical complement of
// [IsNil] and is provided as a convenience [FilterFunc].
func IsNotNil[T any](v T) bool {
	return !IsNil(v)
}
