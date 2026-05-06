/*
 * Copyright (c) 2024 Johan Stenstam, johan.stenstam@internetstiftelsen.se
 */

package cmd

import (
	"bufio"
	"container/heap"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dnstapir/tapir"
	"github.com/miekg/dns"
	"github.com/smhanov/dawg"
	"github.com/spf13/cobra"
)

const textSortChunkBytes = 64 << 20
const progressEveryBytes int64 = 64 << 20
const progressEveryNames = 1_000_000
const progressEveryTime = 5 * time.Second

var dawgSrcFormat, dawgSrcFile, dawgFile, dawgName string

var DawgCmd = &cobra.Command{
	Use:   "dawg",
	Short: "Generate or interact with data stored in a DAWG file; only useable via sub-commands",
}

var dawgCompileCmd = &cobra.Command{
	Use:   "compile",
	Short: "Compile a new DAWG file from either a text or a CSV source file",
	RunE: func(cmd *cobra.Command, args []string) error {
		return compileDawg(dawgSrcFormat, dawgSrcFile, dawgFile)
	},
}

var dawgLookupCmd = &cobra.Command{
	Use:   "lookup",
	Short: "Look up a name in an existing DAWG file",
	RunE: func(cmd *cobra.Command, args []string) error {
		if dawgFile == "" {
			return errors.New("DAWG file not specified")
		}
		if dawgName == "" {
			return errors.New("name to look up not specified")
		}

		fmt.Printf("Loading DAWG: %s\n", dawgFile)
		dawgf, err := dawg.Load(dawgFile)
		if err != nil {
			return fmt.Errorf("load DAWG %q: %w", dawgFile, err)
		}
		defer dawgf.Close()

		name := normalizeDawgName(dawgName)
		idx := dawgf.IndexOf(name)
		if idx == -1 {
			fmt.Printf("Name not found\n")
			return nil
		}

		fmt.Printf("Name %s found, index: %d\n", name, idx)
		return nil
	},
}

var dawgListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all names in an existing DAWG file",
	RunE: func(cmd *cobra.Command, args []string) error {
		if dawgFile == "" {
			return errors.New("DAWG file not specified")
		}

		fmt.Printf("Loading DAWG: %s\n", dawgFile)
		dawgf, err := dawg.Load(dawgFile)
		if err != nil {
			return fmt.Errorf("load DAWG %q: %w", dawgFile, err)
		}
		defer dawgf.Close()

		if tapir.GlobalCF.Debug {
			fmt.Printf("DAWG has %d nodes, %d added and %d edges.\n", dawgf.NumNodes(),
				dawgf.NumAdded(), dawgf.NumEdges())
		}

		count, result := tapir.ListDawg(dawgf)
		fmt.Printf("%v\n", result)
		if tapir.GlobalCF.Verbose {
			fmt.Printf("Enumeration func was called %d times\n", count)
		}
		return nil
	},
}

func init() {
	DawgCmd.AddCommand(dawgCompileCmd, dawgLookupCmd, dawgListCmd)

	DawgCmd.PersistentFlags().StringVarP(&dawgFile, "dawg", "", "",
		"Name of DAWG file, must end in \".dawg\"")
	dawgCompileCmd.Flags().StringVarP(&dawgSrcFormat, "format", "", "",
		"Format of text file, either csv or text")
	dawgCompileCmd.Flags().StringVarP(&dawgSrcFile, "src", "", "",
		"Name of source text file")
	dawgLookupCmd.Flags().StringVarP(&dawgName, "name", "", "",
		"Name to look up")
	dawgListCmd.Flags().StringVarP(&dawgName, "name", "", "",
		"Name to find prefixes of")
}

func compileDawg(srcFormat, srcFile, outfile string) error {
	if srcFile == "" {
		return errors.New("source file not specified")
	}
	if outfile == "" {
		return errors.New("DAWG file not specified")
	}
	if filepath.Ext(outfile) != ".dawg" {
		return fmt.Errorf("DAWG file %q must end in .dawg", outfile)
	}

	if _, err := os.Stat(srcFile); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("source file %q does not exist", srcFile)
		}
		return err
	}

	srcFormat = normalizeSourceFormat(srcFormat, srcFile)
	switch srcFormat {
	case "csv":
		names, err := parseCSVNames(srcFile)
		if err != nil {
			return err
		}
		if len(names) == 0 {
			return fmt.Errorf("source file %q did not contain any domain names", srcFile)
		}

		fmt.Printf("Loaded %d unique names from %s\n", len(names), srcFile)
		return createDawg(names, outfile)
	case "text":
		return createDawgFromText(srcFile, outfile)
	default:
		return fmt.Errorf("format %q of source file %q unknown; must be either csv or text", srcFormat, srcFile)
	}
}

func normalizeSourceFormat(srcFormat, srcFile string) string {
	srcFormat = strings.ToLower(strings.TrimSpace(srcFormat))
	if srcFormat != "" {
		return srcFormat
	}
	if strings.EqualFold(filepath.Ext(srcFile), ".csv") {
		return "csv"
	}
	return "text"
}

func parseCSVNames(srcFile string) ([]string, error) {
	ifd, err := os.Open(srcFile)
	if err != nil {
		return nil, err
	}
	defer ifd.Close()

	info, err := ifd.Stat()
	if err != nil {
		return nil, err
	}
	counter := &countingReader{r: ifd}
	progress := newNameProgress("Reading CSV source", info.Size())
	csvReader := csv.NewReader(counter)
	csvReader.FieldsPerRecord = -1
	csvReader.TrimLeadingSpace = true

	var names []string
	row := 0
	for {
		record, err := csvReader.Read()
		if err == io.EOF {
			break
		}
		row++
		if err != nil {
			return nil, fmt.Errorf("parse CSV %q: %w", srcFile, err)
		}
		if row == 1 && isCSVHeader(record) {
			continue
		}

		name, ok := csvRecordName(record)
		if !ok {
			continue
		}
		name = normalizeDawgName(name)
		if name != "" {
			names = append(names, name)
		}
		progress.maybe(row, len(names), 0, counter.bytes)
	}
	progress.done(row, len(names), 0, counter.bytes)

	fmt.Printf("Sorting %d CSV names\n", len(names))
	return sortUnique(names), nil
}

func isCSVHeader(record []string) bool {
	if len(record) < 2 {
		return false
	}

	first := strings.ToLower(strings.TrimSpace(record[0]))
	second := strings.ToLower(strings.TrimSpace(record[1]))
	rankHeaders := map[string]bool{
		"rank":     true,
		"ranking":  true,
		"position": true,
		"index":    true,
	}
	nameHeaders := map[string]bool{
		"domain": true,
		"fqdn":   true,
		"host":   true,
		"name":   true,
	}

	return rankHeaders[first] && nameHeaders[second]
}

func csvRecordName(record []string) (string, bool) {
	switch len(record) {
	case 0:
		return "", false
	case 1:
		return record[0], true
	default:
		return record[1], true
	}
}

func parseTextNames(srcFile string) ([]string, error) {
	ifd, err := os.Open(srcFile)
	if err != nil {
		return nil, err
	}
	defer ifd.Close()

	var names []string
	scanner := bufio.NewScanner(ifd)
	for scanner.Scan() {
		name := normalizeDawgName(scanner.Text())
		if name != "" {
			names = append(names, name)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return sortUnique(names), nil
}

func createDawgFromText(srcFile, outfile string) error {
	ifd, err := os.Open(srcFile)
	if err != nil {
		return err
	}
	defer ifd.Close()

	fmt.Printf("Creating DAWG data structure from sorted text\n")
	builder := dawg.New()
	scanner := bufio.NewScanner(ifd)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	info, err := ifd.Stat()
	if err != nil {
		return err
	}
	progress := newNameProgress("Building DAWG from sorted text", info.Size())

	var previous string
	var added, skipped, bytesSeen int
	line := 0
	for scanner.Scan() {
		line++
		raw := scanner.Text()
		bytesSeen += len(raw) + 1
		name := normalizeDawgName(raw)
		if name == "" {
			progress.maybe(line, added, skipped, int64(bytesSeen))
			continue
		}
		if name == previous {
			skipped++
			progress.maybe(line, added, skipped, int64(bytesSeen))
			continue
		}
		if previous != "" && name < previous {
			builder = nil
			return createDawgFromUnsortedText(srcFile, outfile, line, previous, name)
		}

		builder.Add(name)
		added++
		previous = name
		if tapir.GlobalCF.Debug {
			fmt.Printf("Added %q to DAWG\n", name)
		}
		progress.maybe(line, added, skipped, int64(bytesSeen))
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if added == 0 {
		return fmt.Errorf("source file %q did not contain any domain names", srcFile)
	}

	if skipped > 0 {
		fmt.Printf("Skipped %d duplicate names from %s\n", skipped, srcFile)
	}
	progress.done(line, added, skipped, int64(bytesSeen))
	return saveDawg(builder, added, outfile)
}

func createDawgFromUnsortedText(srcFile, outfile string, line int, previous, current string) error {
	fmt.Printf("Text source is not sorted after normalization at line %d: %q sorts before %q\n", line, current, previous)
	fmt.Printf("Sorting text source with disk-backed chunks\n")
	return createDawgFromTextSortedOnDisk(srcFile, outfile)
}

func createDawgFromTextSortedOnDisk(srcFile, outfile string) error {
	tempDir, err := os.MkdirTemp("", "dnstapir-dawg-sort-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)

	chunks, err := createSortedTextChunks(srcFile, tempDir)
	if err != nil {
		return err
	}
	if len(chunks) == 0 {
		return fmt.Errorf("source file %q did not contain any domain names", srcFile)
	}

	fmt.Printf("Merging %d sorted chunks into DAWG\n", len(chunks))
	return createDawgFromSortedTextChunks(chunks, outfile)
}

func createSortedTextChunks(srcFile, tempDir string) ([]string, error) {
	ifd, err := os.Open(srcFile)
	if err != nil {
		return nil, err
	}
	defer ifd.Close()
	info, err := ifd.Stat()
	if err != nil {
		return nil, err
	}
	progress := newNameProgress("Scanning text source", info.Size())

	var chunks []string
	chunk := make([]string, 0, 1_000_000)
	chunkBytes := 0
	flushChunk := func() error {
		if len(chunk) == 0 {
			return nil
		}

		chunkInput := len(chunk)
		chunk = sortUnique(chunk)
		path := filepath.Join(tempDir, fmt.Sprintf("chunk-%06d.txt", len(chunks)))
		ofd, err := os.Create(path)
		if err != nil {
			return err
		}
		writer := bufio.NewWriter(ofd)
		for _, name := range chunk {
			if _, err := writer.WriteString(name + "\n"); err != nil {
				ofd.Close()
				return err
			}
		}
		if err := writer.Flush(); err != nil {
			ofd.Close()
			return err
		}
		if err := ofd.Close(); err != nil {
			return err
		}

		chunks = append(chunks, path)
		fmt.Printf("Wrote sorted chunk %d: %d input names, %d unique names\n", len(chunks), chunkInput, len(chunk))
		chunk = make([]string, 0, 1_000_000)
		chunkBytes = 0
		return nil
	}

	scanner := bufio.NewScanner(ifd)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var lines, names int
	var bytesSeen int64
	for scanner.Scan() {
		lines++
		raw := scanner.Text()
		bytesSeen += int64(len(raw) + 1)
		name := normalizeDawgName(raw)
		if name == "" {
			progress.maybe(lines, names, 0, bytesSeen)
			continue
		}
		names++
		chunk = append(chunk, name)
		chunkBytes += len(name) + 1
		if chunkBytes >= textSortChunkBytes {
			if err := flushChunk(); err != nil {
				return nil, err
			}
		}
		progress.maybe(lines, names, 0, bytesSeen)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if err := flushChunk(); err != nil {
		return nil, err
	}
	progress.done(lines, names, 0, bytesSeen)

	return chunks, nil
}

type textChunkScanner struct {
	file    *os.File
	scanner *bufio.Scanner
}

type mergeItem struct {
	name  string
	index int
}

type mergeHeap []mergeItem

func (h mergeHeap) Len() int           { return len(h) }
func (h mergeHeap) Less(i, j int) bool { return h[i].name < h[j].name }
func (h mergeHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *mergeHeap) Push(x any) {
	*h = append(*h, x.(mergeItem))
}

func (h *mergeHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

func createDawgFromSortedTextChunks(chunks []string, outfile string) error {
	var totalBytes int64
	for _, path := range chunks {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		totalBytes += info.Size()
	}

	scanners := make([]textChunkScanner, 0, len(chunks))
	for _, path := range chunks {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		scanners = append(scanners, textChunkScanner{file: file, scanner: scanner})
	}
	defer func() {
		for _, scanner := range scanners {
			scanner.file.Close()
		}
	}()

	items := &mergeHeap{}
	for index := range scanners {
		if scanners[index].scanner.Scan() {
			heap.Push(items, mergeItem{name: scanners[index].scanner.Text(), index: index})
		}
		if err := scanners[index].scanner.Err(); err != nil {
			return err
		}
	}

	builder := dawg.New()
	progress := newNameProgress("Merging sorted chunks into DAWG", totalBytes)
	var previous string
	var added, skipped, processed int
	var bytesSeen int64
	for items.Len() > 0 {
		item := heap.Pop(items).(mergeItem)
		processed++
		bytesSeen += int64(len(item.name) + 1)
		if item.name == previous {
			skipped++
		} else {
			builder.Add(item.name)
			added++
			previous = item.name
			if tapir.GlobalCF.Debug {
				fmt.Printf("Added %q to DAWG\n", item.name)
			}
		}
		progress.maybe(processed, added, skipped, bytesSeen)

		if scanners[item.index].scanner.Scan() {
			heap.Push(items, mergeItem{name: scanners[item.index].scanner.Text(), index: item.index})
		}
		if err := scanners[item.index].scanner.Err(); err != nil {
			return err
		}
	}
	if added == 0 {
		return errors.New("sorted text chunks did not contain any domain names")
	}

	if skipped > 0 {
		fmt.Printf("Skipped %d duplicate names while merging sorted chunks\n", skipped)
	}
	progress.done(processed, added, skipped, bytesSeen)
	return saveDawg(builder, added, outfile)
}

func normalizeDawgName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	return strings.ToLower(dns.Fqdn(name))
}

func sortUnique(names []string) []string {
	slices.Sort(names)
	return slices.Compact(names)
}

func createDawg(sortedNames []string, outfile string) error {
	fmt.Printf("Creating DAWG data structure\n")
	builder := dawg.New()
	progress := newNameProgress("Building DAWG", 0)
	for i, name := range sortedNames {
		builder.Add(name)
		if tapir.GlobalCF.Debug {
			fmt.Printf("Added %q to DAWG\n", name)
		}
		progress.maybe(i+1, i+1, 0, 0)
	}
	progress.done(len(sortedNames), len(sortedNames), 0, 0)

	return saveDawg(builder, len(sortedNames), outfile)
}

func saveDawg(builder dawg.Builder, count int, outfile string) error {
	fmt.Printf("Finalizing DAWG with %d unique names\n", count)
	finder := builder.Finish()
	defer finder.Close()

	fmt.Printf("Finalized DAWG: %d nodes, %d edges\n", finder.NumNodes(), finder.NumEdges())
	fmt.Printf("Saving DAWG to file %s\n", outfile)
	ofd, err := os.Create(outfile)
	if err != nil {
		return err
	}
	progressWriter := newProgressWriter("Saving DAWG")
	progressWriter.writer = ofd
	buffered := bufio.NewWriterSize(progressWriter, 1<<20)
	if _, err := finder.Write(buffered); err != nil {
		ofd.Close()
		return err
	}
	if err := buffered.Flush(); err != nil {
		ofd.Close()
		return err
	}
	if err := ofd.Close(); err != nil {
		return err
	}
	progressWriter.done()
	return nil
}

type countingReader struct {
	r     io.Reader
	bytes int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.bytes += int64(n)
	return n, err
}

type nameProgress struct {
	stage       string
	totalBytes  int64
	started     time.Time
	lastPrinted time.Time
	lastRecords int
	lastBytes   int64
}

func newNameProgress(stage string, totalBytes int64) *nameProgress {
	now := time.Now()
	if totalBytes > 0 {
		fmt.Printf("%s: starting (%s input)\n", stage, formatBytes(totalBytes))
	} else {
		fmt.Printf("%s: starting\n", stage)
	}
	return &nameProgress{
		stage:       stage,
		totalBytes:  totalBytes,
		started:     now,
		lastPrinted: now,
	}
}

func (p *nameProgress) maybe(processed, accepted, skipped int, bytesSeen int64) {
	if processed == 0 {
		return
	}
	now := time.Now()
	if processed-p.lastRecords < progressEveryNames &&
		bytesSeen-p.lastBytes < progressEveryBytes &&
		now.Sub(p.lastPrinted) < progressEveryTime {
		return
	}
	p.print(processed, accepted, skipped, bytesSeen, now)
}

func (p *nameProgress) done(processed, accepted, skipped int, bytesSeen int64) {
	p.print(processed, accepted, skipped, bytesSeen, time.Now())
}

func (p *nameProgress) print(processed, accepted, skipped int, bytesSeen int64, now time.Time) {
	elapsed := now.Sub(p.started).Round(time.Second)
	if p.totalBytes > 0 && bytesSeen > 0 {
		percent := float64(bytesSeen) / float64(p.totalBytes) * 100
		if percent > 100 {
			percent = 100
		}
		fmt.Printf("%s: processed %d, accepted %d, skipped %d, read %s/%s (%.1f%%), elapsed %s\n",
			p.stage, processed, accepted, skipped, formatBytes(bytesSeen), formatBytes(p.totalBytes), percent, elapsed)
	} else {
		fmt.Printf("%s: processed %d, accepted %d, skipped %d, elapsed %s\n",
			p.stage, processed, accepted, skipped, elapsed)
	}
	p.lastPrinted = now
	p.lastRecords = processed
	p.lastBytes = bytesSeen
}

type progressWriter struct {
	writer      io.Writer
	stage       string
	started     time.Time
	lastPrinted time.Time
	written     int64
	lastWritten int64
}

func newProgressWriter(stage string) *progressWriter {
	now := time.Now()
	fmt.Printf("%s: starting\n", stage)
	return &progressWriter{
		stage:       stage,
		started:     now,
		lastPrinted: now,
	}
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	w.written += int64(n)
	w.maybe()
	return n, err
}

func (w *progressWriter) maybe() {
	now := time.Now()
	if w.written-w.lastWritten < progressEveryBytes && now.Sub(w.lastPrinted) < progressEveryTime {
		return
	}
	w.print(now)
}

func (w *progressWriter) done() {
	w.print(time.Now())
}

func (w *progressWriter) print(now time.Time) {
	fmt.Printf("%s: wrote %s, elapsed %s\n", w.stage, formatBytes(w.written), now.Sub(w.started).Round(time.Second))
	w.lastPrinted = now
	w.lastWritten = w.written
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
