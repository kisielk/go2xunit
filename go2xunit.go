package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strings"
	"text/template"
	"unicode"
	"unicode/utf8"
)

// gotest regular expressions
const (
	// === RUN TestAdd
	gtStartRE = "^=== RUN:? ([a-zA-Z_][^[:space:]]*)"

	// --- PASS: TestSub (0.00 seconds)
	// --- FAIL: TestSubFail (0.00 seconds)
	// --- SKIP: TestSubSkip (0.00 seconds)
	gtEndRE = "^--- (PASS|FAIL|SKIP): ([a-zA-Z_][^[:space:]]*) \\((\\d+(.\\d+)?)"

	// FAIL	_/home/miki/Projects/goroot/src/xunit	0.004s
	// ok  	_/home/miki/Projects/goroot/src/anotherTest	0.000s
	gtSuiteRE = "^(ok|FAIL)[ \t]+([^ \t]+)[ \t]+(\\d+.\\d+)"

	// ?       alipay  [no test files]
	gtNoFilesRE = "^\\?.*\\[no test files\\]$"
	// FAIL    node/config [build failed]
	gtBuildFailedRE = `^FAIL.*\[(build|setup) failed\]$`
)

// gocheck regular expressions
const (
	// START: mmath_test.go:16: MySuite.TestAdd
	gcStartRE = "START: [^:]+:[^:]+: ([A-Za-z_][[:word:]]*).([A-Za-z_][[:word:]]*)"
	// PASS: mmath_test.go:16: MySuite.TestAdd	0.000s
	// FAIL: mmath_test.go:35: MySuite.TestDiv
	gcEndRE = "(PASS|FAIL|SKIP|MISS|PANIC): [^:]+:[^:]+: ([A-Za-z_][[:word:]]*).([A-Za-z_][[:word:]]*)([[:space:]]+([0-9]+.[0-9]+))?"
)

const raceRE = "^WARNING: DATA RACE"

type Test struct {
	Name, Time, Message string
	Failed              bool
	Skipped             bool
	Error               bool
}

type Suite struct {
	Name   string
	Time   string
	Status string
	Tests  []*Test
}

type TestResults struct {
	Suites []*Suite
	Multi  bool
}

func (suite *Suite) NumFailed() int {
	count := 0
	for _, test := range suite.Tests {
		if test.Failed {
			count++
		}
	}

	return count
}

func (suite *Suite) NumError() int {
	count := 0
	for _, test := range suite.Tests {
		if test.Error {
			count++
		}
	}
	return count
}

func (suite *Suite) NumSkipped() int {
	count := 0
	for _, test := range suite.Tests {
		if test.Skipped {
			count++
		}
	}

	return count
}

func (suite *Suite) Count() int {
	return len(suite.Tests)
}

func ParseGoTest(rd io.Reader, race bool) ([]*Suite, error) {
	findStart := regexp.MustCompile(gtStartRE).FindStringSubmatch
	findRace := regexp.MustCompile(raceRE).MatchString
	findEnd := regexp.MustCompile(gtEndRE).FindStringSubmatch
	findSuite := regexp.MustCompile(gtSuiteRE).FindStringSubmatch
	isNoFiles := regexp.MustCompile(gtNoFilesRE).MatchString
	isBuildFailed := regexp.MustCompile(gtBuildFailedRE).MatchString
	isExit := regexp.MustCompile("^exit status -?\\d+").MatchString

	suites := []*Suite{}
	var (
		curTest   *Test
		curSuite  *Suite
		out       []string
		foundRace bool
	)

	// Handles a test that ended with a panic.
	handlePanic := func() {
		curTest.Failed = true
		curTest.Skipped = false
		curTest.Time = "N/A"
		curSuite.Tests = append(curSuite.Tests, curTest)
		curTest = nil
	}

	// Appends output to the last test.
	appendError := func() error {
		if len(out) > 0 && curSuite != nil && len(curSuite.Tests) > 0 {
			message := strings.Join(out, "\n")
			if curSuite.Tests[len(curSuite.Tests)-1].Message == "" {
				curSuite.Tests[len(curSuite.Tests)-1].Message = message
			} else {
				curSuite.Tests[len(curSuite.Tests)-1].Message += "\n" + message
			}
		}
		out = []string{}
		return nil
	}

	scanner := NewScanner(rd)
	scanner.Split(scanPrintable)

	for lnum := 1; scanner.Scan(); lnum++ {
		line := scanner.Text()

		// TODO: Only outside a suite/test, report as empty suite?
		if isNoFiles(line) {
			continue
		}

		if isBuildFailed(line) {
			return nil, fmt.Errorf("%d: package build failed: %s", lnum, line)
		}

		if curSuite == nil {
			curSuite = &Suite{}
		}

		tokens := findStart(line)
		if tokens != nil {
			if curTest != nil {
				// This occurs when the last test ended with a panic.
				handlePanic()
			}
			if e := appendError(); e != nil {
				return nil, e
			}
			curTest = &Test{
				Name: tokens[1],
			}
			foundRace = false
			continue
		}

		foundRace = foundRace || (race && findRace(line))

		tokens = findEnd(line)
		if tokens != nil {
			if curTest == nil {
				return nil, fmt.Errorf("%d: orphan end test", lnum)
			}
			if tokens[2] != curTest.Name {
				return nil, fmt.Errorf("%d: name mismatch", lnum)
			}
			curTest.Failed = (tokens[1] == "FAIL" || foundRace)
			curTest.Skipped = (tokens[1] == "SKIP")
			curTest.Time = tokens[3]
			curTest.Message = strings.Join(out, "\n")
			curSuite.Tests = append(curSuite.Tests, curTest)
			curTest = nil
			out = []string{}
			continue
		}

		tokens = findSuite(line)
		if tokens != nil {
			if curTest != nil {
				// This occurs when the last test ended with a panic.
				handlePanic()
			}
			if e := appendError(); e != nil {
				return nil, e
			}
			curSuite.Name = tokens[2]
			curSuite.Time = tokens[3]
			suites = append(suites, curSuite)
			curSuite = nil
			continue
		}

		if isExit(line) || (line == "FAIL") || (line == "PASS") {
			continue
		}

		out = append(out, line)
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return suites, nil
}

func map2arr(m map[string]*Suite) []*Suite {
	arr := make([]*Suite, 0, len(m))
	for _, suite := range m {
		arr = append(arr, suite)
	}

	return arr
}

func scanPrintable(data []byte, atEOF bool) (advance int, token []byte, err error) {
	advance, token, err = ScanLines(data, atEOF)
	if err == nil && token != nil {
		s := make([]byte, 0, len(token))
		for len(token) > 0 {
			r, l := utf8.DecodeRune(token)
			if unicode.IsGraphic(r) || unicode.IsSpace(r) {
				s = append(s, token[:l]...)
			}
			token = token[l:]
		}
		token = s
	}
	return
}

// ParseGoCheck parses output of "go test -gocheck.vv", returns a list of tests
// See data/gocheck.out for an example
func ParseGoCheck(rd io.Reader, race bool) ([]*Suite, error) {
	findStart := regexp.MustCompile(gcStartRE).FindStringSubmatch
	findRace := regexp.MustCompile(raceRE).MatchString
	findEnd := regexp.MustCompile(gcEndRE).FindStringSubmatch

	var (
		test      *Test
		suites    = make(map[string]*Suite)
		suiteName string
		out       []string
		foundRace bool
	)

	scanner := NewScanner(rd)
	scanner.Split(scanPrintable)

	for lnum := 1; scanner.Scan(); lnum++ {
		line := scanner.Text()
		tokens := findStart(line)
		if len(tokens) > 0 {
			if ignoredTests[tokens[2]] {
				continue
			}

			if test != nil {
				return nil, fmt.Errorf("%d: start in middle of %s:%s", lnum, suiteName, test.Name)
			}
			suiteName = tokens[1]
			test = &Test{Name: tokens[2]}
			out = []string{}
			foundRace = false
			continue
		}

		foundRace = foundRace || (race && findRace(line))

		tokens = findEnd(line)
		if len(tokens) > 0 {
			if ignoredTests[tokens[3]] {
				continue
			}
			if test == nil {
				return nil, fmt.Errorf("%d: orphan end", lnum)
			}
			if (tokens[2] != suiteName) || (tokens[3] != test.Name) {
				return nil, fmt.Errorf("%d: suite/name mismatch: got %s:%s, expected %s:%s",
					lnum, tokens[2], tokens[3], suiteName, test.Name)
			}
			test.Message = strings.Join(out, "\n")
			test.Time = strings.TrimSpace(tokens[4])
			test.Failed = (tokens[1] == "FAIL" || tokens[1] == "PANIC" || foundRace)
			test.Error = (tokens[1] == "MISS")
			test.Skipped = (tokens[1] == "SKIP")

			suite, ok := suites[suiteName]
			if !ok {
				suite = &Suite{Name: suiteName}
			}
			suite.Tests = append(suite.Tests, test)
			suites[suiteName] = suite

			test = nil
			suiteName = ""
			out = []string{}

			continue
		}

		if test != nil {
			out = append(out, line)
		}
	}
	return map2arr(suites), scanner.Err()
}

var ignoredTests = map[string]bool{
	"SetUpTest":    true,
	"TearDownTest": true,
}

func hasFailures(suites []*Suite) bool {
	for _, suite := range suites {
		if suite.NumFailed() > 0 {
			return true
		}
	}
	return false
}

var xmlTemplate = template.Must(template.New("xml").Parse(`<?xml version="1.0" encoding="utf-8"?>
{{if .Multi}}<testsuites>{{end}}
{{range $suite := .Suites}}  <testsuite name="{{.Name}}" tests="{{.Count}}" errors="{{.NumError}}" failures="{{.NumFailed}}" skip="{{.NumSkipped}}">
{{range  $test := $suite.Tests}}    <testcase classname="{{$suite.Name}}" name="{{$test.Name}}" time="{{$test.Time}}">
{{if $test.Skipped }}      <skipped/> {{end}}
{{if $test.Error }}      <error/> {{end}}
{{if $test.Failed }}      <failure type="go.error" message="error">
        <![CDATA[{{$test.Message}}]]>
      </failure>{{end}}    </testcase>
{{end}}  </testsuite>
{{end}}{{if .Multi}}</testsuites>{{end}}
`))

// writeXML exits xunit XML of tests to out
func writeXML(suites []*Suite, out io.Writer, bamboo bool) error {
	testsResult := TestResults{
		Suites: suites,
		Multi:  bamboo || (len(suites) > 1),
	}
	return xmlTemplate.Execute(out, testsResult)
}

// getInput return input io.Reader from file name, if file name is - it will
// return os.Stdin
func getInput(filename string) (io.Reader, error) {
	if filename == "-" || filename == "" {
		return os.Stdin, nil
	}

	return os.Open(filename)
}

// getInput return output io.Writer from file name, if file name is - it will
// return os.Stdout
func getOutput(filename string) (io.Writer, error) {
	if filename == "-" || filename == "" {
		return os.Stdout, nil
	}

	return os.Create(filename)
}

// getIO returns input and output streams from file names
func getIO(inputFile, outputFile string) (io.Reader, io.Writer, error) {
	input, err := getInput(inputFile)
	if err != nil {
		return nil, nil, fmt.Errorf("can't open %s for reading: %s", inputFile, err)
	}

	output, err := getOutput(outputFile)
	if err != nil {
		return nil, nil, fmt.Errorf("can't open %s for writing: %s", outputFile, err)
	}

	return input, output, nil
}

func main() {
	inputFile := flag.String("input", "", "input file (default to stdin)")
	outputFile := flag.String("output", "", "output file (default to stdout)")
	fail := flag.Bool("fail", false, "fail (non zero exit) if any test failed")
	bamboo := flag.Bool("bamboo", false, "xml compatible with Atlassian's Bamboo")
	gocheck := flag.Bool("gocheck", false, "parse gocheck output")
	race := flag.Bool("race", false, "mark tests with data races as failed")
	flag.Parse()

	// No time ... prefix for error messages
	log.SetFlags(0)

	if flag.NArg() > 0 {
		log.Fatalf("error: %s does not take parameters (did you mean -input?)", os.Args[0])
	}

	input, output, err := getIO(*inputFile, *outputFile)
	if err != nil {
		log.Fatalf("error: %s", err)
	}

	parse := ParseGoTest
	if *gocheck {
		parse = ParseGoCheck
	}

	suites, err := parse(input, *race)
	if err != nil {
		log.Fatalf("error: %s", err)
	}
	if len(suites) == 0 {
		log.Fatalf("error: no tests found")
		os.Exit(1)
	}

	err = writeXML(suites, output, *bamboo)
	if err != nil {
		log.Fatalln("error writing output:", err)
	}
	if *fail && hasFailures(suites) {
		os.Exit(1)
	}
}
