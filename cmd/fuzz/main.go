package fuzz

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/fd0/termstatus"
	"github.com/happal/monsoon/cli"
	"github.com/happal/monsoon/producer"
	"github.com/happal/monsoon/recorder"
	"github.com/happal/monsoon/reporter"
	"github.com/happal/monsoon/request"
	"github.com/happal/monsoon/response"
	"github.com/happal/monsoon/shell"
	"github.com/spf13/cobra"
)

// Options collect options for a run.
type Options struct {
	Range       string
	RangeFormat string
	Filename    string
	Logfile     string
	Logdir      string
	Threads     int

	RequestsPerSecond float64

	BufferSize int
	Skip       int
	Limit      int

	Request        *request.Request // the template for the HTTP request
	FollowRedirect int

	HideStatusCodes []int
	HideHeaderSize  []string
	HideBodySize    []string
	HidePattern     []string
	hidePattern     []*regexp.Regexp
	ShowPattern     []string
	showPattern     []*regexp.Regexp

	Extract        []string
	extract        []*regexp.Regexp
	ExtractPipe    []string
	extractPipe    [][]string
	BodyBufferSize int
}

var opts Options

func compileRegexps(pattern []string) (res []*regexp.Regexp, err error) {
	for _, pat := range pattern {
		r, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("regexp %q failed to compile: %v", pat, err)
		}

		res = append(res, r)
	}

	return res, nil
}

func splitShell(cmds []string) ([][]string, error) {
	var data [][]string
	for _, cmd := range cmds {
		args, err := shell.Split(cmd)
		if err != nil {
			return nil, err
		}
		if len(args) < 1 {
			return nil, fmt.Errorf("invalid command: %q", cmd)
		}
		data = append(data, args)
	}
	return data, nil
}

// valid validates the options and returns an error if something is invalid.
func (opts *Options) valid() (err error) {
	if opts.Range != "" && opts.Filename != "" {
		return errors.New("only one source allowed but both range and filename specified")
	}

	opts.extract, err = compileRegexps(opts.Extract)
	if err != nil {
		return err
	}

	opts.extractPipe, err = splitShell(opts.ExtractPipe)
	if err != nil {
		return err
	}

	opts.hidePattern, err = compileRegexps(opts.HidePattern)
	if err != nil {
		return err
	}

	opts.showPattern, err = compileRegexps(opts.ShowPattern)
	if err != nil {
		return err
	}

	return nil
}

var cmd = &cobra.Command{
	Use: "fuzz [options] URL",
	DisableFlagsInUseLine: true,

	Short:   helpShort,
	Long:    helpLong,
	Example: helpExamples,

	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.WithContext(func(ctx context.Context, g *errgroup.Group) error {
			return run(ctx, g, &opts, args)
		})
	},
}

// AddCommand adds the 'run' command to cmd.
func AddCommand(c *cobra.Command) {
	c.AddCommand(cmd)

	fs := cmd.Flags()
	fs.SortFlags = false

	fs.StringVarP(&opts.Range, "range", "r", "", "set range `from-to`")
	fs.StringVar(&opts.RangeFormat, "range-format", "%d", "set `format` for range")

	fs.StringVarP(&opts.Filename, "file", "f", "", "read values from `filename`")
	fs.StringVar(&opts.Logfile, "logfile", "", "write copy of printed messages to `filename`.log")
	fs.StringVar(&opts.Logdir, "logdir", os.Getenv("MONSOON_LOG_DIR"), "automatically log all output to files in `dir`")

	fs.IntVarP(&opts.Threads, "threads", "t", 5, "make as many as `n` parallel requests")
	fs.IntVar(&opts.BufferSize, "buffer-size", 100000, "set number of buffered items to `n`")
	fs.IntVar(&opts.Skip, "skip", 0, "skip the first `n` requests")
	fs.IntVar(&opts.Limit, "limit", 0, "only run `n` requests, then exit")
	fs.Float64Var(&opts.RequestsPerSecond, "requests-per-second", 0, "do at most `n` requests per minute (e.g. 0.5)")

	// add all options to define a request
	opts.Request = request.New("")
	request.AddFlags(opts.Request, fs)

	fs.IntVar(&opts.FollowRedirect, "follow-redirect", 0, "follow `n` redirects")

	fs.IntSliceVar(&opts.HideStatusCodes, "hide-status", nil, "hide responses with this status `code,[code],[...]`")
	fs.StringSliceVar(&opts.HideHeaderSize, "hide-header-size", nil, "hide responses with this header size (`size,from-to,from-,-to`)")
	fs.StringSliceVar(&opts.HideBodySize, "hide-body-size", nil, "hide responses with this body size (`size,from-to,from-,-to`)")
	fs.StringArrayVar(&opts.HidePattern, "hide-pattern", nil, "hide responses containing `regex` in response header or body (can be specified multiple times)")
	fs.StringArrayVar(&opts.ShowPattern, "show-pattern", nil, "show only responses containing `regex` in response header or body (can be specified multiple times)")

	fs.StringArrayVar(&opts.Extract, "extract", nil, "extract `regex` from response body (can be specified multiple times)")
	fs.StringArrayVar(&opts.ExtractPipe, "extract-pipe", nil, "pipe response body to `cmd` to extract data (can be specified multiple times)")
	fs.IntVar(&opts.BodyBufferSize, "body-buffer-size", 5, "use `n` MiB as the buffer size for extracting strings from a response body")
}

// logfilePath returns the prefix for the logfiles, if any.
func logfilePath(opts *Options, inputURL string) (prefix string, err error) {
	if opts.Logdir != "" && opts.Logfile == "" {
		url, err := url.Parse(inputURL)
		if err != nil {
			return "", err
		}

		ts := time.Now().Format("20060102_150405")
		fn := fmt.Sprintf("monsoon_%s_%s", url.Host, ts)
		p := filepath.Join(opts.Logdir, fn)
		return p, nil
	}

	return opts.Logfile, nil
}

func setupProducer(ctx context.Context, g *errgroup.Group, opts *Options, ch chan<- string, count chan<- int) error {
	switch {
	case opts.Range != "":
		var first, last int
		_, err := fmt.Sscanf(opts.Range, "%d-%d", &first, &last)
		if err != nil {
			return errors.New("wrong format for range, expected: first-last")
		}

		g.Go(func() error {
			return producer.Range(ctx, first, last, opts.RangeFormat, ch, count)
		})
		return nil

	case opts.Filename == "-":
		g.Go(func() error {
			return producer.Reader(ctx, os.Stdin, ch, count)
		})
		return nil

	case opts.Filename != "":
		file, err := os.Open(opts.Filename)
		if err != nil {
			return err
		}

		g.Go(func() error {
			return producer.Reader(ctx, file, ch, count)
		})
		return nil

	default:
		return errors.New("neither file nor range specified, nothing to do")
	}
}

func setupTerminal(ctx context.Context, g *errgroup.Group, logfilePrefix string) (term cli.Terminal, cleanup func(), err error) {
	ctx, cancel := context.WithCancel(ctx)

	if logfilePrefix != "" {
		fmt.Printf("logfile is %s.log\n", logfilePrefix)

		logfile, err := os.Create(logfilePrefix + ".log")
		if err != nil {
			return nil, cancel, err
		}

		fmt.Fprintln(logfile, shell.Join(os.Args))

		// write copies of messages to logfile
		term = &cli.LogTerminal{
			Terminal: termstatus.New(os.Stdout, os.Stderr, false),
			Writer:   logfile,
		}
	} else {
		term = termstatus.New(os.Stdout, os.Stderr, false)
	}

	// make sure error messages logged via the log package are printed nicely
	w := cli.NewStdioWrapper(term)
	log.SetOutput(w.Stderr())

	g.Go(func() error {
		term.Run(ctx)
		return nil
	})

	return term, cancel, nil
}

func setupResponseFilters(opts *Options) ([]response.Filter, error) {
	filters := []response.Filter{
		response.NewFilterStatusCode(opts.HideStatusCodes),
	}

	if len(opts.HideHeaderSize) > 0 || len(opts.HideBodySize) > 0 {
		f, err := response.NewFilterSize(opts.HideHeaderSize, opts.HideBodySize)
		if err != nil {
			return nil, err
		}
		filters = append(filters, f)
	}

	if len(opts.hidePattern) > 0 {
		filters = append(filters, response.FilterRejectPattern{Pattern: opts.hidePattern})
	}

	if len(opts.showPattern) > 0 {
		filters = append(filters, response.FilterAcceptPattern{Pattern: opts.showPattern})
	}

	return filters, nil
}

func setupValueFilters(ctx context.Context, opts *Options, valueCh <-chan string, countCh <-chan int) (<-chan string, <-chan int) {
	if opts.Skip > 0 {
		f := &producer.FilterSkip{Skip: opts.Skip}
		countCh = f.Count(ctx, countCh)
		valueCh = f.Select(ctx, valueCh)
	}

	if opts.Limit > 0 {
		f := &producer.FilterLimit{Max: opts.Limit}
		countCh = f.Count(ctx, countCh)
		valueCh = f.Select(ctx, valueCh)
	}

	return valueCh, countCh
}

func startRunners(ctx context.Context, opts *Options, in <-chan string) <-chan response.Response {
	out := make(chan response.Response)

	var wg sync.WaitGroup
	transport := response.NewTransport(opts.Request.Insecure)
	for i := 0; i < opts.Threads; i++ {
		runner := response.NewRunner(transport, opts.Request, in, out)
		runner.BodyBufferSize = opts.BodyBufferSize * 1024 * 1024
		runner.Extract = opts.extract
		runner.ExtractPipe = opts.extractPipe

		runner.Client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) <= opts.FollowRedirect {
				return nil
			}
			return http.ErrUseLastResponse
		}
		wg.Add(1)
		go func() {
			runner.Run(ctx)
			wg.Done()
		}()
	}

	go func() {
		// wait until the runners are done, then close the output channel
		wg.Wait()
		close(out)
	}()

	return out
}

func run(ctx context.Context, g *errgroup.Group, opts *Options, args []string) error {
	// make sure the options and arguments are valid
	if len(args) == 0 {
		return errors.New("last argument needs to be the URL")
	}

	if len(args) > 1 {
		return errors.New("more than one target URL specified")
	}

	err := opts.valid()
	if err != nil {
		return err
	}

	inputURL := args[0]
	opts.Request.URL = inputURL

	// setup logging and the terminal
	logfilePrefix, err := logfilePath(opts, inputURL)
	if err != nil {
		return err
	}

	term, cleanup, err := setupTerminal(ctx, g, logfilePrefix)
	defer cleanup()
	if err != nil {
		return err
	}

	// collect the filters for the responses
	responseFilters, err := setupResponseFilters(opts)
	if err != nil {
		return err
	}

	// setup the pipeline for the values
	vch := make(chan string, opts.BufferSize)
	var valueCh <-chan string = vch
	cch := make(chan int, 1)
	var countCh <-chan int = cch

	// start a producer from the options
	err = setupProducer(ctx, g, opts, vch, cch)
	if err != nil {
		return err
	}

	// filter values (skip, limit)
	valueCh, countCh = setupValueFilters(ctx, opts, valueCh, countCh)

	// limit the throughput (if requested)
	if opts.RequestsPerSecond > 0 {
		valueCh = producer.Limit(ctx, opts.RequestsPerSecond, valueCh)
	}

	// start the runners
	responseCh := startRunners(ctx, opts, valueCh)

	// filter the responses
	responseCh = response.Mark(responseCh, responseFilters)

	if logfilePrefix != "" {
		rec, err := recorder.New(logfilePrefix+".json", opts.Request, opts.Extract, opts.ExtractPipe)
		if err != nil {
			return err
		}

		out := make(chan response.Response)
		in := responseCh
		responseCh = out

		outCount := make(chan int)
		inCount := countCh
		countCh = outCount

		g.Go(func() error {
			return rec.Run(ctx, in, out, inCount, outCount)
		})
	}

	// run the reporter
	term.Printf("input URL %v\n\n", inputURL)
	reporter := reporter.New(term)
	return reporter.Display(responseCh, countCh)
}
