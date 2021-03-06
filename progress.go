package mpb

import (
	"container/heap"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"sync"
	"time"

	"github.com/vbauerster/mpb/cwriter"
)

const (
	// default RefreshRate
	prr = 120 * time.Millisecond
	// default width
	pwidth = 80
	// default format
	pformat = "[=>-]"
)

// Progress represents the container that renders Progress bars
type Progress struct {
	wg           *sync.WaitGroup
	uwg          *sync.WaitGroup
	operateState chan func(*pState)
	done         chan struct{}
	refresh      chan struct{}
}

type pState struct {
	bHeap           *priorityQueue
	shutdownPending []*Bar
	heapUpdated     bool
	zeroWait        bool
	idCounter       int
	width           int
	format          string
	rr              time.Duration
	cw              *cwriter.Writer
	ticker          *time.Ticker
	pMatrix         map[int][]chan int
	aMatrix         map[int][]chan int

	// following are provided by user
	uwg              *sync.WaitGroup
	cancel           <-chan struct{}
	shutdownNotifier chan struct{}
	waitBars         map[*Bar]*Bar
	debugOut         io.Writer
}

// New creates new Progress instance, which orchestrates bars rendering process.
// Accepts mpb.ProgressOption funcs for customization.
func New(options ...ProgressOption) *Progress {
	pq := make(priorityQueue, 0)
	heap.Init(&pq)
	s := &pState{
		bHeap:    &pq,
		width:    pwidth,
		format:   pformat,
		cw:       cwriter.New(os.Stdout),
		rr:       prr,
		ticker:   time.NewTicker(prr),
		waitBars: make(map[*Bar]*Bar),
		debugOut: ioutil.Discard,
	}

	for _, opt := range options {
		if opt != nil {
			opt(s)
		}
	}

	p := &Progress{
		uwg:          s.uwg,
		wg:           new(sync.WaitGroup),
		operateState: make(chan func(*pState)),
		done:         make(chan struct{}),
		refresh:      make(chan struct{}),
	}
	go p.serve(s)
	return p
}

// AddBar creates a new progress bar and adds to the container.
func (p *Progress) AddBar(total int64, options ...BarOption) *Bar {
	p.wg.Add(1)
	result := make(chan *Bar)
	select {
	case p.operateState <- func(s *pState) {
		options = append(options, barWidth(s.width), barFormat(s.format))
		b := newBar(p.wg, s.idCounter, total, s.cancel, options...)
		if b.runningBar != nil {
			s.waitBars[b.runningBar] = b
		} else {
			heap.Push(s.bHeap, b)
			s.heapUpdated = true
		}
		s.idCounter++
		result <- b
	}:
		return <-result
	case <-p.done:
		p.wg.Done()
		return nil
	}
}

// Abort is only effective while bar progress is running,
// it means remove bar now without waiting for its completion.
// If bar is already completed, there is nothing to abort.
// If you need to remove bar after completion, use BarRemoveOnComplete BarOption.
func (p *Progress) Abort(b *Bar, remove bool) {
	select {
	case p.operateState <- func(s *pState) {
		if b.index < 0 {
			return
		}
		if remove {
			s.heapUpdated = heap.Remove(s.bHeap, b.index) != nil
		}
		s.shutdownPending = append(s.shutdownPending, b)
	}:
	case <-p.done:
	}
}

// UpdateBarPriority provides a way to change bar's order position.
// Zero is highest priority, i.e. bar will be on top.
func (p *Progress) UpdateBarPriority(b *Bar, priority int) {
	select {
	case p.operateState <- func(s *pState) { s.bHeap.update(b, priority) }:
	case <-p.done:
	}
}

// BarCount returns bars count
func (p *Progress) BarCount() int {
	result := make(chan int, 1)
	select {
	case p.operateState <- func(s *pState) { result <- s.bHeap.Len() }:
		return <-result
	case <-p.done:
		return 0
	}
}

// Refresh will refresh the progress bars manually instead of waiting
// the refresh rate timer.
func (p *Progress) Refresh() {
	select {
	case p.refresh <- struct{}{}:
		return
	case <-p.done:
		return
	}
}

// Wait first waits for user provided *sync.WaitGroup, if any,
// then waits far all bars to complete and finally shutdowns master goroutine.
// After this method has been called, there is no way to reuse *Progress instance.
func (p *Progress) Wait() {
	if p.uwg != nil {
		p.uwg.Wait()
	}

	p.wg.Wait()

	select {
	case p.operateState <- func(s *pState) { s.zeroWait = true }:
		// manually refresh to flush state if we have a really long
		// configured refresh rate
		p.Refresh()
		<-p.done
	case <-p.done:
	}
}

func (s *pState) updateSyncMatrix() {
	s.pMatrix = make(map[int][]chan int)
	s.aMatrix = make(map[int][]chan int)
	for i := 0; i < s.bHeap.Len(); i++ {
		bar := (*s.bHeap)[i]
		table := bar.wSyncTable()
		pRow, aRow := table[0], table[1]

		for i, ch := range pRow {
			s.pMatrix[i] = append(s.pMatrix[i], ch)
		}

		for i, ch := range aRow {
			s.aMatrix[i] = append(s.aMatrix[i], ch)
		}
	}
}

func (s *pState) render(tw int) {
	if s.heapUpdated {
		s.updateSyncMatrix()
		s.heapUpdated = false
	}
	syncWidth(s.pMatrix)
	syncWidth(s.aMatrix)

	for i := 0; i < s.bHeap.Len(); i++ {
		bar := (*s.bHeap)[i]
		go bar.render(s.debugOut, tw)
	}

	if err := s.flush(); err != nil {
		fmt.Fprintf(s.debugOut, "%s %s %v\n", "[mpb]", time.Now(), err)
	}
}

func (s *pState) flush() (err error) {
	for s.bHeap.Len() > 0 {
		bar := heap.Pop(s.bHeap).(*Bar)
		reader := <-bar.frameReaderCh
		if _, e := s.cw.ReadFrom(reader); e != nil {
			err = e
		}
		defer func() {
			if frame, ok := reader.(*frameReader); ok && frame.toShutdown {
				// shutdown at next flush, in other words decrement underlying WaitGroup
				// only after the bar with completed state has been flushed.
				// this ensures no bar ends up with less than 100% rendered.
				s.shutdownPending = append(s.shutdownPending, bar)
				if replacementBar, ok := s.waitBars[bar]; ok {
					heap.Push(s.bHeap, replacementBar)
					s.heapUpdated = true
					delete(s.waitBars, bar)
				}
				if frame.removeOnComplete {
					s.heapUpdated = true
					return
				}
			}
			heap.Push(s.bHeap, bar)
		}()
	}

	if e := s.cw.Flush(); err == nil {
		err = e
	}

	for i := len(s.shutdownPending) - 1; i >= 0; i-- {
		close(s.shutdownPending[i].shutdown)
		s.shutdownPending = s.shutdownPending[:i]
	}
	return
}

func syncWidth(matrix map[int][]chan int) {
	for _, column := range matrix {
		column := column
		go func() {
			var maxWidth int
			for _, ch := range column {
				w := <-ch
				if w > maxWidth {
					maxWidth = w
				}
			}
			for _, ch := range column {
				ch <- maxWidth
			}
		}()
	}
}
