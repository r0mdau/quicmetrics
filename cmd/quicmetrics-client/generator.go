package main

import (
	"fmt"
	"math"
	"math/rand/v2"
)

// functionNames are the FaaS functions the generator simulates, most popular first.
var functionNames = []string{
	"checkout", "auth", "search", "resize-image", "send-email",
	"render-pdf", "ingest-events", "notify", "export-csv", "billing-sync",
}

type weighted struct {
	value  string
	weight int
}

var (
	regions = []weighted{
		{"us-east-1", 50}, {"eu-west-1", 35}, {"ap-southeast-1", 15},
	}
	methods = []weighted{
		{"GET", 70}, {"POST", 20}, {"PUT", 6}, {"DELETE", 4},
	}
	statuses = []weighted{
		{"200", 85}, {"201", 5}, {"404", 6}, {"500", 3}, {"503", 1},
	}
	errorKinds = []weighted{
		{"timeout", 50}, {"unhandled", 35}, {"oom", 15},
	}
)

// maxConcurrency caps the simulated in-flight executions of one function in one region.
const maxConcurrency = 100

// generator produces a stream of DogStatsD metrics that looks like the traffic of a small
// FaaS platform: a few popular functions, a long tail, mostly successful requests and a
// trickle of errors, as counters, plus gauges for in-flight executions and memory use and
// long-tailed histograms for execution time and request latency, in milliseconds.
// All randomness comes from one seed so a run can be replayed.
type generator struct {
	rng         *rand.Rand
	functions   []weighted
	concurrency map[string]int
}

// newGenerator simulates the first functions of functionNames, which run must have checked
// to be between 1 and len(functionNames).
func newGenerator(seed uint64, functions int) *generator {
	fns := make([]weighted, functions)
	for i, name := range functionNames[:functions] {
		fns[i] = weighted{name, 100 / (i + 1)}
	}
	return &generator{
		rng:         rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), //nolint:gosec // simulated traffic, not security sensitive
		functions:   fns,
		concurrency: map[string]int{},
	}
}

// next returns one DogStatsD counter, gauge or histogram line.
func (g *generator) next() string {
	fn := g.pick(g.functions)
	region := g.pick(regions)
	switch roll := g.rng.IntN(100); {
	case roll < 32:
		cold := g.rng.IntN(100) < 5
		return fmt.Sprintf("faas.invocations:%d|c|#function:%s,region:%s,cold_start:%t",
			1+g.rng.IntN(5), fn, region, cold)
	case roll < 61:
		return fmt.Sprintf("http.requests:%d|c|#function:%s,region:%s,method:%s,status:%s",
			1+g.rng.IntN(10), fn, region, g.pick(methods), g.pick(statuses))
	case roll < 69:
		return fmt.Sprintf("faas.billed.ms:%d|c|#function:%s,region:%s",
			10+int(g.rng.ExpFloat64()*120), fn, region)
	case roll < 72:
		return fmt.Sprintf("faas.errors:1|c|#function:%s,region:%s,error:%s",
			fn, region, g.pick(errorKinds))
	case roll < 79:
		return fmt.Sprintf("faas.concurrency:%d|g|#function:%s,region:%s",
			g.drift(fn+"/"+region), fn, region)
	case roll < 85:
		return fmt.Sprintf("faas.memory.utilization:%.3f|g|#function:%s,region:%s",
			min(max(0.5+0.15*g.rng.NormFloat64(), 0), 1), fn, region)
	case roll < 94:
		ms := g.logNormal(80, 1)
		if g.rng.IntN(100) < 5 {
			ms *= 4
		}
		return fmt.Sprintf("faas.duration:%.2f|h|#function:%s,region:%s", ms, fn, region)
	default:
		return fmt.Sprintf("http.latency:%.2f|ms|#function:%s,region:%s,method:%s",
			g.logNormal(30, 1.1), fn, region, g.pick(methods))
	}
}

// logNormal returns a positive value with the given median whose logarithm has a standard
// deviation of sigma: most values sit near the median and a few are many times larger.
func (g *generator) logNormal(median, sigma float64) float64 {
	return median * math.Exp(sigma*g.rng.NormFloat64())
}

// drift moves the in-flight executions of a series by at most one, so the gauge wanders
// instead of jumping around.
func (g *generator) drift(series string) int {
	n, ok := g.concurrency[series]
	if !ok {
		n = 1 + g.rng.IntN(10)
	}
	n = min(max(n+g.rng.IntN(3)-1, 0), maxConcurrency)
	g.concurrency[series] = n
	return n
}

func (g *generator) pick(choices []weighted) string {
	total := 0
	for _, c := range choices {
		total += c.weight
	}
	n := g.rng.IntN(total)
	for _, c := range choices {
		if n < c.weight {
			return c.value
		}
		n -= c.weight
	}
	return choices[len(choices)-1].value
}
