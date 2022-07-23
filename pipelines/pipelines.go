// Package pipelines provides helper functions for constructing concurrent processing pipelines.
// Each pipeline stage represents a single stage in a parallel computation, with an input channel and an output channel.
// Generally, pipeline stages have signatures starting with a context and input channel as their first arguments, and
// returning a channel, as below:
//
//   Stage[S,T any](ctx context.Context, in <-chan S, ...) <-chan T
//
// The return value from a pipeline stage is referred to as the stage's 'output' channel. Each stage is a non-blocking
// call which starts one or more goroutines which listen on the input channel and send results to the output channel.
// Goroutines started by each stage respond to context cancellation or closure of the input channel by closing its
// output channel and cleaning up all goroutines started. Many pipeline stages take a function as an argument, which
// transform the input and output in some way.
//
// By default, each pipeline starts the minimum number of threads required for its operation, and returns an unbuffered channel.
// These defaults can be modified by passing the result of WithBuffer or WithPool as optional arguments.
package pipelines

import (
	"context"
	"sync"
)

type config[T any] struct {
	channer    func() chan T
	workers    int
	outputs    int
	doneCancel context.CancelFunc
}

func (c config[T]) makeOutputs() []chan T {
	var result []chan T
	for i := 0; i < c.outputs; i++ {
		result = append(result, c.channer())
	}
	return result
}

// sendOnly casts an array of channels to receive-only channels.
func sendOnly[T any](chans []chan T) []chan<- T {
	result := make([]chan<- T, len(chans))
	for i, ch := range chans {
		result[i] = ch
	}
	return result
}

// recvOnly casts an array of channels to send-only channels
func recvOnly[T any](chans []chan T) []<-chan T {
	result := make([]<-chan T, len(chans))
	for i, ch := range chans {
		result[i] = ch
	}
	return result
}

// An OptionFunc is passed to optionally configure a pipeline stage.
type OptionFunc[T any] func(*config[T])

// WithBuffer configures a pipeline to return a buffered output channel with a buffer of the provided size.s
func WithBuffer[T any](size int) OptionFunc[T] {
	return func(conf *config[T]) {
		conf.channer = func() chan T {
			return make(chan T, size)
		}
	}
}

// WithPool configures a pipeline to run the provided stage on a parallel worker pool of the given size. All workers are
// kept alive until the input channel is closed or the provided context is cancelled.
func WithPool[T any](numWorkers int) OptionFunc[T] {
	return func(conf *config[T]) {
		conf.workers = numWorkers
	}
}

// WithDone returns a context which is cancelled when all goroutines started by this pipeline stage have shutdown.
func WithDone[T any](ctx context.Context) (OptionFunc[T], context.Context) {
	ctx, cancel := context.WithCancel(context.Background())
	return func(conf *config[T]) {
		conf.doneCancel = cancel
	}, ctx
}

func configure[T any](opts []OptionFunc[T]) config[T] {
	result := config[T]{
		channer: func() chan T { return make(chan T) },
		workers: 1,
		outputs: 1,
	}
	for _, opt := range opts {
		opt(&result)
	}
	return result
}

// Chan converts a slice of type T to a buffered channel containing the same values. Unlike other funcs in this package,
// Chan does not start any new goroutines.
func Chan[T any](in []T) <-chan T {
	result := make(chan T, len(in))
	defer close(result) // non-empty buffered channels can be drained even when closed.
	for _, t := range in {
		result <- t
	}
	return result
}

// Flatten provides a pipeline stage which converts a channel of slices to a channel of scalar values.
// Each value contained in slices received from the input channel is sent to the output channel.
func Flatten[T any](ctx context.Context, in <-chan []T, opts ...OptionFunc[T]) <-chan T {
	return return1(doWithConf(ctx, func(ctx context.Context, out ...chan<- T) {
		doFlatten(ctx, in, out[0])
	}, configure(opts)))
}

func doFlatten[T any](ctx context.Context, in <-chan []T, result chan<- T) {
	for {
		select {
		case <-ctx.Done():
			return
		case t, ok := <-in:
			if !ok {
				return
			}
			sendAll(ctx, t, result)
		}
	}
}

// Map applies f to every value received from the input channel and sends the result to the output channel.
// The output channel is closed when the input channel is closed or the provided context is cancelled.
func Map[S, T any](ctx context.Context, in <-chan S, f func(S) T, opts ...OptionFunc[T]) <-chan T {
	return return1(doWithConf(ctx, func(ctx context.Context, out ...chan<- T) {
		doMap(ctx, in, f, out[0])
	}, configure(opts)))
}

func return1[T any](chans []<-chan T) <-chan T {
	return chans[0]
}

func return2[T any](chans []<-chan T) (<-chan T, <-chan T) {
	return chans[0], chans[1]
}

func doMap[S, T any](ctx context.Context, in <-chan S, f func(S) T, result chan<- T) {
	for {
		select {
		case <-ctx.Done():
			return
		case s, ok := <-in:
			if !ok {
				return
			}
			select {
			case <-ctx.Done():
				return
			case result <- f(s):
			}
		}
	}
}

// MapCtx applies f to every value received from its input channel and sends the result to its output channel.
// The same context passed to MapCtx is passed as an argument to f.
func MapCtx[S, T any](ctx context.Context, in <-chan S, f func(context.Context, S) T, opts ...OptionFunc[T]) <-chan T {
	return return1(doWithConf(ctx, func(ctx context.Context, out ...chan<- T) {
		doMapCtx(ctx, in, f, out[0])
	}, configure(opts)))
}

func doMapCtx[S, T any](ctx context.Context, in <-chan S, f func(context.Context, S) T, result chan<- T) {
	for {
		select {
		case <-ctx.Done():
			return
		case s, ok := <-in:
			if !ok {
				return
			}
			select {
			case <-ctx.Done():
				return
			case result <- f(ctx, s):
			}
		}
	}
}

// FlatMap applies f to every value received from its input channel and sends all values found in the slice returned from
// f to its output channel.
func FlatMap[S, T any](ctx context.Context, in <-chan S, f func(S) []T, opts ...OptionFunc[T]) <-chan T {
	return return1(doWithConf(ctx, func(ctx context.Context, out ...chan<- T) {
		doFlatMap(ctx, in, f, out[0])
	}, configure(opts)))
}

func doFlatMap[S, T any](ctx context.Context, in <-chan S, f func(S) []T, out chan<- T) {
	for {
		select {
		case <-ctx.Done():
			return
		case s, ok := <-in:
			if !ok {
				return
			}
			sendAll(ctx, f(s), out)
		}
	}
}

// FlatMapCtx applies f to every value received from its input channel and sends all values found in the slice returned from
// f to its output channel.
// The same context passed to FlatMapCtx is passed as an argument to f.
func FlatMapCtx[S, T any](ctx context.Context, in <-chan S, f func(context.Context, S) []T, opts ...OptionFunc[T]) <-chan T {
	return return1(doWithConf(ctx, func(ctx context.Context, out ...chan<- T) {
		doFlatMapCtx(ctx, in, f, out[0])
	}, configure(opts)))
}

func doFlatMapCtx[S, T any](ctx context.Context, in <-chan S, f func(context.Context, S) []T, out chan<- T) {
	for {
		select {
		case <-ctx.Done():
			return
		case s, ok := <-in:
			if !ok {
				return
			}
			sendAll(ctx, f(ctx, s), out)
		}
	}
}

// Combine sends all values received from both of its input channels to its output channel.
func Combine[T any](ctx context.Context, t1 <-chan T, t2 <-chan T, opts ...OptionFunc[T]) <-chan T {
	return return1(doWithConf(ctx, func(ctx context.Context, out ...chan<- T) {
		doCombine(ctx, t1, t2, out[0])
	}, configure(opts)))
}

func doCombine[T any](ctx context.Context, t1 <-chan T, t2 <-chan T, out chan<- T) {
	closedCount := 0
	for closedCount < 2 {
		select {
		case <-ctx.Done():
			return
		case v, ok := <-t1:
			if !ok {
				t1 = nil
				closedCount++
				continue
			}
			out <- v
		case v, ok := <-t2:
			if !ok {
				t2 = nil
				closedCount++
				continue
			}
			out <- v
		}
	}
}

// Tee sends all values received from the input channel into 2 output channels. Both output channels must be
// drained simultaneously to avoid blocking this pipeline stage.
func Tee[T any](ctx context.Context, ch <-chan T, opts ...OptionFunc[T]) (<-chan T, <-chan T) {
	conf := configure(opts)
	conf.outputs = 2
	return return2(doWithConf(ctx, func(ctx context.Context, out ...chan<- T) {
		doTee(ctx, ch, out[0], out[1])
	}, conf))
}

func doTee[T any](ctx context.Context, ch <-chan T, chan1, chan2 chan<- T) {
	for {
		select {
		case t, ok := <-ch:
			if !ok {
				return
			}
			select {
			case chan1 <- t:
				select {
				case chan2 <- t:
				case <-ctx.Done():
					return
				}
			case chan2 <- t:
				select {
				case chan1 <- t:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// WithCancel passes each value received from its input channel to its output channel.
// If the provided context is cancelled or the input channel is closed, the output channel is also closed.
func WithCancel[T any](ctx context.Context, ch <-chan T, opts ...OptionFunc[T]) <-chan T {
	return return1(doWithConf(ctx, func(ctx context.Context, out ...chan<- T) {
		doWithCancel(ctx, ch, out[0])
	}, configure(opts)))
}

func doWithCancel[T any](ctx context.Context, ch <-chan T, out chan<- T) {
	for {
		select {
		case <-ctx.Done():
			return
		case t, ok := <-ch:
			if !ok {
				return
			}
			select {
			case <-ctx.Done():
				return
			case out <- t:
			}
		}
	}
}

// OptionMap applies f to every value received from in and sends all non-nil results to its output channel.
func OptionMap[S, T any](ctx context.Context, in <-chan S, f func(S) *T, opts ...OptionFunc[T]) <-chan T {
	return return1(doWithConf(ctx, func(ctx context.Context, out ...chan<- T) {
		doOptionMap(ctx, in, out[0], f)
	}, configure(opts)))
}

func doOptionMap[S, T any](ctx context.Context, in <-chan S, out chan<- T, f func(S) *T) {
	for {
		select {
		case <-ctx.Done():
			return
		case s, ok := <-in:
			if !ok {
				return
			}
			t := f(s)
			if t == nil {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case out <- *t:
			}
		}
	}
}

// OptionMapCtx applies f to every value received from in and sends all non-nil results to its output channel.
// The same context passed to OptionMapCtx is passed as an argument to f.
func OptionMapCtx[S, T any](ctx context.Context, in <-chan S, f func(context.Context, S) *T, opts ...OptionFunc[T]) <-chan T {
	return return1(doWithConf(ctx, func(ctx context.Context, out ...chan<- T) {
		doOptionMapCtx(ctx, in, out[0], f)
	}, configure(opts)))
}

func doOptionMapCtx[S, T any](ctx context.Context, in <-chan S, out chan<- T, f func(context.Context, S) *T) {
	for {
		select {
		case <-ctx.Done():
			return
		case s, ok := <-in:
			if !ok {
				return
			}
			t := f(ctx, s)
			if t == nil {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case out <- *t:
			}
		}
	}
}

// sendAll sends all values in a slice to the provided channel. It blocks until the channel is closed or the provided
// context is cancelled.
func sendAll[T any](ctx context.Context, ts []T, ch chan<- T) {
	for _, t := range ts {
		select {
		case <-ctx.Done():
			return
		case ch <- t:
		}
	}
}

// ForkMapCtx forks an invocation of f onto a new goroutine for each value received from in.
// f is passed the output channel directly, and is responsible for sending its output to this channel.
// To avoid resource leaks, f must respect context cancellation when sending to its output channel.
// The same context passed to ForkMapCtx is passed to f.
//
// ForkMapCtx should be used with caution, as it introduces potentially unbounded parallelism to a pipeline computation.
//
// Variants of ForkMapCtx are intentionally omitted from this package.
// ForkMap is omitted because the caller cannot listen for context cancellation in some cases.
// ForkFlatMap is omitted because it is more efficient for the caller range over the slice and send individual values themselves.
func ForkMapCtx[S, T any](ctx context.Context, in <-chan S, f func(context.Context, S, chan<- T), opts ...OptionFunc[T]) <-chan T {
	return return1(doWithConf(ctx, func(ctx context.Context, out ...chan<- T) {
		doForkMapCtx(ctx, in, f, out[0])
	}, configure(opts)))
}

func doForkMapCtx[S, T any](ctx context.Context, in <-chan S, f func(context.Context, S, chan<- T), out chan<- T) {
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		select {
		case <-ctx.Done():
			return
		case s, ok := <-in:
			if !ok {
				return
			}
			wg.Add(1)
			go func(s S) {
				defer wg.Done()
				f(ctx, s, out)
			}(s)
		}
	}
}

// doWithConf runs the implementation provided via doIt on goroutines according to the provided options.
func doWithConf[T any](ctx context.Context, doIt func(context.Context, ...chan<- T), conf config[T]) []<-chan T {
	outs := conf.makeOutputs()
	if conf.workers == 1 {
		go func() { // avoid any overhead from the WaitGroup.
			if conf.doneCancel != nil {
				defer conf.doneCancel()
			}
			defer func() {
				for _, ch := range outs {
					close(ch)
				}
			}()
			doIt(ctx, sendOnly(outs)...)
		}()
	} else {
		var wg sync.WaitGroup // run on worker pool
		for i := 0; i < conf.workers; i++ {
			wg.Add(1)
			go func(id int) {
				defer func() {
					wg.Done()
					if id == 0 { // first thread closes the output channel.
						wg.Wait()
						defer func() {
							for _, ch := range outs {
								close(ch)
							}
						}()
						if conf.doneCancel != nil {
							conf.doneCancel()
						}
					}
				}()
				doIt(ctx, sendOnly(outs)...)
			}(i)
		}
	}
	return recvOnly(outs)
}

// doOnPool performs the function doIt on a pool of size poolSize, closing all channels passed to outs when the
// invocation of doIt completes.
func doOnPool[T any](ctx context.Context, poolSize int, doIt func(context.Context, ...chan<- T), outs ...chan<- T) {
	closeAll := func() {
		for _, ch := range outs {
			close(ch)
		}
	}
	if poolSize == 1 {
		go func() {
			defer closeAll()
			doIt(ctx, outs...)
		}()
	} else {
		var wg sync.WaitGroup
		for i := 0; i < poolSize; i++ {
			wg.Add(1)
			go func(id int) {
				defer func() {
					wg.Done()
					if id == 0 { // first thread closes the output channel.
						wg.Wait()
						closeAll()
					}
				}()
				doIt(ctx, outs...)
			}(i)
		}
	}
}

// Drain receives all values from the provided channel and returns them in a slice.
// Drain blocks the caller until the input channel is closed or the provided context is cancelled.
// An error is returned if and only if the provided context was cancelled before the input channel was closed.
func Drain[T any](ctx context.Context, in <-chan T) ([]T, error) {
	var result []T
	for {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case repo, ok := <-in:
			if !ok {
				return result, nil
			}
			result = append(result, repo)
		}
	}
}

// Reduce runs a reducer function on every input received from the in chan and returns the output. Reduce blocks the
// caller until the input channel is closed or the provided context is cancelled.
// An error is returned if and only if the provided context was cancelled before the input channel was closed.
func Reduce[S, T string](ctx context.Context, in <-chan S, f func(T, S) T) (T, error) {
	var result T
	for {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case s, ok := <-in:
			if !ok {
				return result, nil
			}
			result = f(result, s)
		}
	}
}
