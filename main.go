package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"go.uber.org/zap"
)

var flagRegion = flag.String("region", "", "The AWS region to query")

func main() {
	flag.Parse()
	log, err := zap.NewProduction()
	if err != nil {
		panic(fmt.Sprintf("could not create log: %v", err))
	}

	// Handle Ctrl-C.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	// Create a cancellable context and wire it up to signals from Ctrl-C.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-signals
		fmt.Println()
		cancel()
	}()

	// Set up the AWS SDK.
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatal("could not load AWS config", zap.Error(err))
	}
	if flagRegion != nil && *flagRegion != "" {
		cfg.Region = *flagRegion
	}
	log = log.With(zap.String("region", cfg.Region))

	// Find current account.
	log.Info("Looking up account ID")
	identity, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		log.Fatal("could not get current identity, are you logged in?", zap.Error(err))
	}
	log = log.With(zap.String("account", *identity.Account))

	// Create the file name used to store the data.
	outputFileName := fmt.Sprintf("%s-%s.json", *identity.Account, cfg.Region)

	// Run the report.
	var functionReports []FunctionReports
	// If the data doesn't exist on disk, get it and cache it.
	if _, err := os.Stat(outputFileName); err != nil {
		log.Info("no existing report data found, downloading logs from AWS")
		functionReports, err = getFunctionReports(ctx, log, cfg)
		if err != nil {
			log.Fatal("failed to get function reports", zap.Error(err))
		}
		log.Info("creating report JSON file")
		f, err := os.Create(outputFileName)
		if err != nil {
			log.Fatal("could not create report JSON file", zap.Error(err))
		}
		defer f.Close()
		err = json.NewEncoder(f).Encode(functionReports)
		if err != nil {
			log.Fatal("could not export JSON", zap.Error(err))
		}
		log.Info("downloading logs complete")
	} else {
		log.Info("existing report data found, using it", zap.String("filename", outputFileName))
		// Now that the data is found, display the results.
		input, err := os.Open(outputFileName)
		if err != nil {
			log.Fatal("could not get open output.json", zap.Error(err))
		}
		err = json.NewDecoder(input).Decode(&functionReports)
		if err != nil {
			log.Fatal("could not get decode output.json", zap.Error(err))
		}
	}

	// Display the results.
	displayReport(functionReports)
}

func displayReport(reportContent []FunctionReports) {
	sort.Slice(reportContent, func(i, j int) bool {
		a := reportContent[i].Cost()
		b := reportContent[j].Cost()
		return a > b
	})
	tw := tabwriter.NewWriter(os.Stdout, 1, 1, 1, ' ', 0)
	fmt.Fprintln(tw, strings.Join([]string{
		"Name",
		"Arch",
		"Daily",
		"Monthly",
		"Invocations",
		"Avg",             // Duration
		"RAM",             // Max
		"RAM",             // Assigned
		"RAM",             // Optimal)
		"Monthly Savings", // arm64 + RAM
	}, "\t"))
	fmt.Fprintln(tw, strings.Join([]string{
		"",
		"",
		"",
		"",
		"",
		"Duration", // Avg
		"Max",      // RAM
		"Assigned", // RAM
		"Optimal",  // RAM
		"(arm64 + RAM)",
	}, "\t"))
	for _, rc := range reportContent {
		var pcUsed float64
		if rc.MemoryAssigned() > 0 {
			pcUsed = (float64(rc.MaxMemoryUsed()) / float64(rc.MemoryAssigned())) * 100.0
		}
		cost := rc.Cost()
		optimisedRAM, optimisedCost := rc.OptimisedCost()
		optimisedRAMDisplay := fmt.Sprintf("%d", optimisedRAM)
		if optimisedRAM == 0 {
			optimisedRAMDisplay = "N/A"
		}
		monthlySavings := (cost * 30) - (optimisedCost * 30)
		if monthlySavings < 0 {
			monthlySavings = 0.0
		}
		fmt.Fprintln(tw, strings.Join([]string{
			rc.Name,
			rc.Architecture,
			fmt.Sprintf("$%.5f", cost),
			fmt.Sprintf("$%.5f", cost*30),
			fmt.Sprintf("%d", len(rc.Reports)),
			fmt.Sprintf("%v", rc.AvgDuration()),
			fmt.Sprintf("%d (%.2f%%)", rc.MaxMemoryUsed(), pcUsed),
			fmt.Sprintf("%d", rc.MemoryAssigned()),
			optimisedRAMDisplay,
			fmt.Sprintf("$%.2f", monthlySavings),
		}, "\t"))
	}
	tw.Flush()
	return
}

func getFunctionReports(ctx context.Context, log *zap.Logger, cfg aws.Config) (functionReports []FunctionReports, err error) {
	// Get functions.
	log.Info("Listing functions")
	lambdaClient := lambda.NewFromConfig(cfg)
	lambdaFunctions, err := getLambdaFunctions(ctx, lambdaClient)
	if err != nil {
		log.Fatal("could not load functions", zap.Error(err))
	}
	log = log.With(zap.Int("functionCount", len(lambdaFunctions)))
	log.Info("Found functions")

	// Get log streams for each log group.
	cwLogsClient := cloudwatchlogs.NewFromConfig(cfg)

	// Create the function functionReports.
	functionReports = make([]FunctionReports, len(lambdaFunctions))
	for i := range lambdaFunctions {
		f := lambdaFunctions[i]
		functionReports[i].Name = *f.FunctionName
		var architectures []string
		for ia := range f.Architectures {
			architectures = append(architectures, string(f.Architectures[ia]))
		}
		functionReports[i].Architecture = strings.Join(architectures, " ")
	}

	// Download the log streams.
	log.Info("Downloading logs")
	end := time.Now()
	start := end.Add(time.Hour * -24)
	var logEventCount int
	var invocationCount int
	for i := range lambdaFunctions {
		logGroupName := fmt.Sprintf("/aws/lambda/%s", *lambdaFunctions[i].FunctionName)
		log.Info("Downloading logs", zap.String("functionName", *lambdaFunctions[i].FunctionName), zap.Int("functionIndex", i))
		logEventsPaginator := cloudwatchlogs.NewFilterLogEventsPaginator(cwLogsClient, &cloudwatchlogs.FilterLogEventsInput{
			LogGroupName: &logGroupName,
			StartTime:    aws.Int64(start.UnixMilli()),
			EndTime:      aws.Int64(end.UnixMilli()),
		})
		var page *cloudwatchlogs.FilterLogEventsOutput
		for logEventsPaginator.HasMorePages() {
			page, err = logEventsPaginator.NextPage(ctx)
			if err != nil {
				log.Error("getLogStreams: failed to get next page", zap.Error(err), zap.String("functionName", *lambdaFunctions[i].FunctionName))
				break
			}
			for ei := range page.Events {
				event := page.Events[ei]
				r, ok, err := getFunctionReport(*event.Message)
				if err != nil {
					log.Error("getLogStreams: failed to get report", zap.Error(err), zap.String("functionName", *lambdaFunctions[i].FunctionName), zap.String("logMessage", *event.Message))
					continue
				}
				logEventCount++
				if logEventCount%10000 == 0 {
					log.Info("Working", zap.Int("logEventCount", logEventCount), zap.Int("invocationCount", invocationCount))
				}
				if !ok {
					continue
				}
				functionReports[i].Reports = append(functionReports[i].Reports, r)
				invocationCount++
			}
		}
	}
	log.Info("Downloading log data complete", zap.Int("logEventCount", logEventCount), zap.Int("invocationCount", invocationCount))
	return functionReports, nil
}

type FunctionReports struct {
	Name         string   `json:"name"`
	Architecture string   `json:"architecture"`
	Reports      []Report `json:"reports"`
}

/*
x86 Price
	First 6 Billion GB-seconds / month	$0.0000166667 for every GB-second	$0.20 per 1M requests
	Next 9 Billion GB-seconds / month	$0.000015 for every GB-second	$0.20 per 1M requests
	Over 15 Billion GB-seconds / month	$0.0000133334 for every GB-second	$0.20 per 1M requests
Arm Price
	First 7.5 Billion GB-seconds / month	$0.0000133334 for every GB-second	$0.20 per 1M requests
	Next 11.25 Billion GB-seconds / month	$0.0000120001 for every GB-second	$0.20 per 1M requests
	Over 18.75 Billion GB-seconds / month	$0.0000106667 for every GB-second	$0.20 per 1M requests
*/

const M = 1000000

func (fr FunctionReports) AvgDuration() (v time.Duration) {
	if len(fr.Reports) == 0 {
		return
	}
	var count int
	for _, r := range fr.Reports {
		v += r.Duration
		count++
	}
	return v / time.Duration(count)
}

func (fr FunctionReports) AvgMemoryUsed() (v int64) {
	if len(fr.Reports) == 0 {
		return
	}
	var count int64
	for _, r := range fr.Reports {
		v += r.MaxMemoryUsed
		count++
	}
	return v / count
}

func (fr FunctionReports) MaxMemoryUsed() (v int64) {
	for _, r := range fr.Reports {
		if v < r.MaxMemoryUsed {
			v = r.MaxMemoryUsed
		}
	}
	return
}

func (fr FunctionReports) MemoryAssigned() int64 {
	if len(fr.Reports) == 0 {
		return 0
	}
	return fr.Reports[0].MemorySize
}

// Minimum RAM assigned to a Lambda function.
const minRAM = 1024

func (fr FunctionReports) OptimisedCost() (memSize int64, cost float64) {
	if len(fr.Reports) == 0 {
		return
	}
	memSize = fr.Reports[0].MemorySize
	// Don't bother optimising below the minimum amount of RAM.
	if memSize > minRAM {
		// Select double the RAM that's ever been required.
		proposedMemSize := fr.MaxMemoryUsed() * 2
		// Use at least the minimum amount of RAM.
		if proposedMemSize < minRAM {
			proposedMemSize = minRAM + 1
		}
		// Round down to nearest 256MB chunk.
		proposedMemSize = (proposedMemSize / 256) * 256
		// Only choose less RAM.
		if proposedMemSize < memSize {
			memSize = proposedMemSize
		}
	}
	return memSize, fr.CostForArchitecture("arm64", memSize)
}

func (fr FunctionReports) Cost() (cost float64) {
	return fr.CostForArchitecture(fr.Architecture, 0)
}

func (fr FunctionReports) CostForArchitecture(architecture string, memorySize int64) (cost float64) {
	if len(fr.Reports) == 0 {
		return 0.0
	}
	costPer1MRequests := 0.20
	costForRequests := costPer1MRequests / M * float64(len(fr.Reports))
	var msBilled time.Duration
	for _, r := range fr.Reports {
		msBilled += r.BilledDuration
		if memorySize == 0 {
			memorySize = r.MemorySize
		}
	}
	gbSecondPrice := 0.0000166667
	if architecture == "arm64" {
		gbSecondPrice = 0.0000133334
	}
	secs := msBilled.Seconds()
	gbs := float64(memorySize) / 1024.0
	cost = (gbs * secs * gbSecondPrice) + costForRequests
	return
}

type Report struct {
	RequestID      string        `json:"requestId"`
	Duration       time.Duration `json:"duration"`
	BilledDuration time.Duration `json:"billedDuration"`
	InitDuration   time.Duration `json:"initDuration"`
	MemorySize     int64         `json:"memorySize"`
	MaxMemoryUsed  int64         `json:"maxMemoryUsed"`
	IsColdStart    bool          `json:"isColdStart"`
}

func parseMS(v string) (d time.Duration, err error) {
	return time.ParseDuration(strings.Replace(v, " ms", "ms", -1))
}

func parseMB(v string) (mb int64, err error) {
	return strconv.ParseInt(strings.Replace(v, " MB", "", -1), 10, 64)
}

func getFunctionReport(report string) (r Report, ok bool, err error) {
	report = strings.TrimSpace(report)
	if !strings.HasPrefix(report, "REPORT") {
		return
	}
	ok = true
	parts := strings.Split(report, "\t")
	for _, p := range parts {
		kv := strings.SplitN(p, ": ", 2)
		if len(kv) > 1 {
			v := strings.TrimSpace(kv[1])
			switch strings.TrimSpace(kv[0]) {
			case "RequestId":
				r.RequestID = v
			case "Duration":
				r.Duration, err = parseMS(v)
				if err != nil {
					err = fmt.Errorf("could not parse duration: %q: %w", v, err)
					return
				}
			case "Billed Duration":
				r.BilledDuration, err = parseMS(v)
				if err != nil {
					err = fmt.Errorf("could not parse billed duration: %q: %w", v, err)
					return
				}
			case "Memory Size":
				r.MemorySize, err = parseMB(v)
				if err != nil {
					err = fmt.Errorf("could not parse memory size: %q: %w", v, err)
					return
				}
			case "Max Memory Used":
				r.MaxMemoryUsed, err = parseMB(v)
				if err != nil {
					err = fmt.Errorf("could not parse max memory used: %q: %w", v, err)
					return
				}
			case "Init Duration":
				r.InitDuration, err = parseMS(v)
				if err != nil {
					err = fmt.Errorf("could not parse init duration: %q: %w", v, err)
					return
				}
				r.IsColdStart = true
			}
		}
	}
	return
}

// REPORT RequestId: d432a1bd-8320-4fad-95d5-290fc6ea9f02	Duration: 27.83 ms	Billed Duration: 28 ms	Memory Size: 3096 MB	Max Memory Used: 62 MB

// REPORT RequestId: e6ef2bbc-cc60-4a4e-a671-915a809e05d3	Duration: 1365.00 ms	Billed Duration: 1618 ms	Memory Size: 3096 MB	Max Memory Used: 55 MB	Init Duration: 252.99 ms
// XRAY TraceId: 1-62f6637f-27b6ec11099249663df0fc13	SegmentId: 69ccfd435d559a96	Sampled: true

func getLambdaFunctions(ctx context.Context, lambdaClient *lambda.Client) (functions []types.FunctionConfiguration, err error) {
	lambdaFunctionPaginator := lambda.NewListFunctionsPaginator(lambdaClient, &lambda.ListFunctionsInput{})
	var page *lambda.ListFunctionsOutput
	for lambdaFunctionPaginator.HasMorePages() {
		page, err = lambdaFunctionPaginator.NextPage(ctx)
		if err != nil {
			err = fmt.Errorf("getLambdaFunctions: failed to get next page: %w", err)
			return
		}

		// Log the objects found
		for i := range page.Functions {
			functions = append(functions, page.Functions[i])
		}
	}
	return
}
