package mux

import (
	"reflect"
)

type FilterFunc[T any] func(T) bool

type MapperFunc[T, U any] func(T) U

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

func Any[T any]() FilterFunc[T] {
	return func(T) bool {
		return true
	}
}

func Not[T any](filter FilterFunc[T]) FilterFunc[T] {
	return func(v T) bool {
		return !filter(v)
	}
}

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

func IsNotNil[T any](v T) bool {
	return !IsNil(v)
}
