// Copyright © 2019 Oxford Nanopore Technologies.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package cmd

import (
	"fmt"
	"github.com/fsnotify/fsnotify"
	"github.com/iafan/cwalk"
	"github.com/shenwei356/xopen"
	"github.com/spf13/cobra"
	"os"
	"os/signal"
	ospath "path"
	"regexp"
	"runtime"
	"sync"
	"syscall"
	"time"
)

type WatchCtrl int

type WatchCtrlChan chan WatchCtrl

func reFilterName(name string, re *regexp.Regexp) bool {
	return re.MatchString(name)
}

// scatCmd represents the fish command
var scatCmd = &cobra.Command{
	Use:   "scat",
	Short: "look for short sequences in larger sequences using local alignment",
	Long:  "look for short sequences in larger sequences using local alignment",

	Run: func(cmd *cobra.Command, args []string) {
		config := getConfigs(cmd)
		outFile := config.OutFile
		runtime.GOMAXPROCS(config.Threads)

		qBase := getFlagPositiveInt(cmd, "qual-ascii-base")
		inFmt := getFlagString(cmd, "in-format")
		outFmt := getFlagString(cmd, "out-format")
		dropString := getFlagString(cmd, "drop-time")
		allowGaps := getFlagBool(cmd, "allow-gaps")
		timeLimit := getFlagString(cmd, "time-limit")
		var ticker time.Timer
		if timeLimit != "" {
			timeout, err := time.ParseDuration(timeLimit)
			checkError(err)
			tmp := time.NewTimer(timeout)
			ticker = *tmp
		}
		waitPid := getFlagInt(cmd, "wait-pid")
		delta := getFlagInt(cmd, "delta") * 1024
		reStr := getFlagString(cmd, "regexp")
		var err error
		reFilter, err := regexp.Compile(reStr)
		checkError(err)

		dirs := getFileList(args, true)
		outfh, err := xopen.Wopen(outFile)
		checkError(err)
		defer outfh.Close()
		ctrlChan := make(WatchCtrlChan)
		ndirs := []string{}
		for _, d := range dirs {
			if d != "-" {
				ndirs = append(ndirs, d)
			}
		}
		if len(ndirs) == 0 {
			log.Info("No directories given to watch! Exiting.")
			os.Exit(1)
		}
		LaunchFxWatchers(dirs, ctrlChan, reFilter, inFmt, outFmt, qBase, allowGaps, delta, &ticker, dropString, waitPid)

	},
}

func LaunchFxWatchers(dirs []string, ctrlChan WatchCtrlChan, re *regexp.Regexp, inFmt, outFmt string, qBase int, allowGaps bool, delta int, ticker *time.Timer, dropString string, waitPid int) {
	allSeqChans := make([]chan *simpleSeq, len(dirs))
	allCtrlChans := make([]WatchCtrlChan, len(dirs))
	for i, dir := range dirs {
		allSeqChans[i] = make(chan *simpleSeq, 10000)
		allCtrlChans[i] = make(WatchCtrlChan, 0)
		go NewFxWatcher(dir, allSeqChans[i], allCtrlChans[i], re, inFmt, outFmt, qBase, allowGaps, delta, dropString)
	}
	sigChan := make(chan os.Signal, 5)
	pidTimer := *time.NewTicker(time.Second * 2)
	if waitPid < 0 {
		pidTimer.C = nil
	} else {
		log.Info("Running until process with PID", waitPid, "exits.")
	}
	signal.Notify(sigChan, os.Interrupt)

	for {
		select {
		case <-sigChan:
			signal.Stop(sigChan)
			for i, cc := range allCtrlChans {
				if cc == nil {
					continue
				}
				cc <- WatchCtrl(i)
				<-cc
			}
			return
		case <-pidTimer.C:
			killErr := syscall.Kill(waitPid, syscall.Signal(0))
			if killErr != nil {
				log.Info("Watched process with PID", waitPid, "exited.")
				for i, cc := range allCtrlChans {
					if cc == nil {
						continue
					}
					cc <- WatchCtrl(i)
					<-cc
				}
				return
			}
		default:
			active := 0
			for j, sc := range allSeqChans {
				if sc == nil {
					continue
				}
				active++
			PULL:
				for {
					select {
					case seq := <-sc:
						fmt.Println(seq)
					case fb := <-allCtrlChans[j]:
						if fb != WatchCtrl(-9) {
							log.Fatal("Invalid command:", fb)
						}
						allSeqChans[j] = nil
						allCtrlChans[j] = nil
					default:
						break PULL
					}
				}
			}
			if active == 0 {
				for i, cc := range allCtrlChans {
					if cc == nil || allSeqChans[i] == nil {
						continue
					}
					cc <- WatchCtrl(i)
					<-cc
				}
				return
			}
		}
	}
}

type WatchedFx struct {
	Name        string
	LastSize    int64
	LastTry     time.Time
	BytesRead   int64
	IsDir       bool
	SeqChan     chan *simpleSeq
	CtrlChanIn  chan SeqStreamCtrl
	CtrlChanOut chan SeqStreamCtrl
}

type WatchedFxPool map[string]*WatchedFx

type FxWatcher struct {
	Base  string
	Pool  WatchedFxPool
	Mutex sync.Mutex
}

func NewFxWatcher(dir string, seqChan chan *simpleSeq, ctrlChan WatchCtrlChan, re *regexp.Regexp, inFmt, outFmt string, qBase int, allowGaps bool, minDelta int, dropString string) {
	sigChan := make(chan os.Signal, 5)
	signal.Notify(sigChan, os.Interrupt)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalf("fsnotify error:", err)
	}
	defer watcher.Close()
	self := new(FxWatcher)
	self.Base = dir
	self.Pool = make(WatchedFxPool)
	dropDuration, err := time.ParseDuration(dropString)
	checkError(err)

	walkFunc := func(path string, info os.FileInfo, err error) error {
		path = ospath.Join(dir, path)
		self.Mutex.Lock()
		if self.Pool[path] != nil {
			self.Mutex.Unlock()
			return nil
		}
		if info.IsDir() {
			err = watcher.Add(path)
			checkError(err)
			self.Pool[path] = &WatchedFx{Name: path, IsDir: true}
			log.Info("Watching directory:", path)
		} else {
			if !reFilterName(path, re) {
				return nil
			}
			created := time.Now()
			sc := seqChan
			ctrlIn, ctrlOut := NewRawSeqStreamFromFile(path, sc, qBase, inFmt, allowGaps)
			err := watcher.Add(path)
			checkError(err)
			fi, err := os.Stat(path)
			checkError(err)
			self.Pool[path] = &WatchedFx{Name: path, IsDir: false, SeqChan: sc, CtrlChanIn: ctrlIn, CtrlChanOut: ctrlOut, LastSize: fi.Size(), LastTry: created}

			ctrlIn <- StreamTry
			log.Info("Watching file:", path)
		}
		self.Mutex.Unlock()
		return nil
	}

	go func() {
	SFOR:
		for {
			select {
			case <-ctrlChan:
				self.Mutex.Lock()
				for ePath, w := range self.Pool {
					watcher.Remove(ePath)
					if w.IsDir {
						log.Info("Stopped watching directory: ", ePath)
						delete(self.Pool, ePath)
						continue
					}
					log.Info("Stopped watching file: ", ePath)
					w.CtrlChanIn <- StreamQuit
					for j := range w.CtrlChanOut {
						if j == StreamExited {
							break
						} else if j != StreamEOF {
							log.Fatal("Invalid command:", int(j))
						}
					}
					delete(self.Pool, ePath)
				}
				self.Mutex.Unlock()
				log.Info("Exiting.")
				ctrlChan <- WatchCtrl(-9)
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				ePath := event.Name
				if event.Op&fsnotify.Remove == fsnotify.Remove {
					self.Mutex.Lock()
					di := self.Pool[ePath]
					if di == nil {
						self.Mutex.Unlock()
						continue SFOR
					}
					if di.IsDir {
						log.Info("Removed directory:", ePath)
						watcher.Remove(ePath)
						delete(self.Pool, ePath)
						self.Mutex.Unlock()
						continue SFOR
					}
					log.Info("Removed file:", ePath)
					di.CtrlChanIn <- StreamQuit
					fb := <-di.CtrlChanOut
					if fb != StreamExited {
						log.Fatal("Invalid command:", int(fb))
					}
					delete(self.Pool, ePath)
					self.Mutex.Unlock()
					continue SFOR
				}
				if event.Op&fsnotify.Rename == fsnotify.Rename {
					log.Info("Stopped watching renamed file:", ePath)
					self.Pool[ePath].CtrlChanIn <- StreamQuit
					fb := <-self.Pool[ePath].CtrlChanOut
					if fb != StreamExited {
						log.Fatal("Invalid command:", int(fb))
					}
					delete(self.Pool, ePath)
				}
				if event.Op&fsnotify.Create == fsnotify.Create {
					if self.Pool[ePath] != nil {
						continue
					}
					fi, err := os.Stat(ePath)
					checkError(err)
					self.Mutex.Lock()
					if fi.IsDir() {
						log.Info("Watching new directory:", ePath)
						err := watcher.Add(ePath)
						checkError(err)
						self.Pool[ePath] = &WatchedFx{Name: ePath, IsDir: true}
						self.Mutex.Unlock()
						err = cwalk.Walk(dir, walkFunc)
						checkError(err)
						continue SFOR

					}
					log.Info("Skip:", ePath)
					if !reFilterName(ePath, re) {
						self.Mutex.Unlock()
						continue SFOR
					}
					created := time.Now()

					sc := seqChan
					ctrlIn, ctrlOut := NewRawSeqStreamFromFile(ePath, sc, qBase, inFmt, allowGaps)
					self.Pool[ePath] = &WatchedFx{Name: ePath, IsDir: false, SeqChan: sc, CtrlChanIn: ctrlIn, CtrlChanOut: ctrlOut, LastSize: fi.Size(), LastTry: created}
					err = watcher.Add(ePath)
					checkError(err)
					log.Info("Watching new file:", ePath)
					ctrlIn <- StreamTry
					self.Mutex.Unlock()
				}
				if event.Op&fsnotify.Write == fsnotify.Write {
					fi, err := os.Stat(ePath)
					checkError(err)
					self.Mutex.Lock()
					if self.Pool[ePath] == nil || fi.IsDir() {
						self.Mutex.Unlock()
						continue SFOR
					}
					if time.Now().Sub(self.Pool[ePath].LastTry) < dropDuration {
						self.Mutex.Unlock()
						continue SFOR
					}
					if self.Pool[ePath].LastSize == fi.Size() {
						self.Mutex.Unlock()
						continue SFOR
					}
					delta := fi.Size() - self.Pool[ePath].LastSize
					if delta < 0 {
						log.Info("Stopped watching truncated file:", ePath)
						self.Pool[ePath].CtrlChanIn <- StreamQuit
						fb := <-self.Pool[ePath].CtrlChanOut
						if fb != StreamExited {
							log.Fatal("Invalid command:", int(fb))
						}
						delete(self.Pool, ePath)
						self.Mutex.Unlock()
						continue SFOR
					}
					if delta < int64(minDelta) {
						self.Mutex.Unlock()
						continue SFOR
					}
					self.Pool[ePath].CtrlChanIn <- StreamTry
					self.Pool[ePath].LastTry = time.Now()
					self.Mutex.Unlock()

				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Fatalf("fsnotify error:", err)
			default:

			}
		}
	}()

	self.Mutex.Lock()
	err = watcher.Add(dir)
	self.Pool[dir] = &WatchedFx{Name: dir, IsDir: true}
	checkError(err)
	log.Info(fmt.Sprintf("Watcher (%s) launched on root: %s", inFmt, dir))
	self.Mutex.Unlock()

	err = cwalk.Walk(dir, walkFunc)
	checkError(err)

	for {
		for ePath, w := range self.Pool {
			self.Mutex.Lock()
		CHAN:
			for {
				select {
				case seq, ok := <-w.SeqChan:
					if !ok {
						break CHAN
					}
					fmt.Println(seq)
				case e, ok := <-w.CtrlChanOut:
					if !ok {
						break CHAN
					}
					if e == StreamEOF {
						break CHAN
					}
					if e == StreamExited {
						delete(self.Pool, ePath)
						break CHAN
					} else {
						log.Fatal("Invalid command:", int(e))
					}
				default:
					break CHAN
				}
			}
			fi, err := os.Stat(ePath)
			if err != nil {
				w.LastSize = fi.Size()
			}
			self.Mutex.Unlock()
		}
	}

}

func init() {
	RootCmd.AddCommand(scatCmd)

	scatCmd.Flags().StringP("regexp", "r", ".*\\.(fastq|fq)", "regexp for waxtched files')")
	scatCmd.Flags().StringP("in-format", "I", "fastq", "input format: fastq or fasta (fastq)")
	scatCmd.Flags().StringP("out-format", "O", "fastq", "output format: fastq or fasta")
	scatCmd.Flags().BoolP("allow-gaps", "A", false, "allow gap character (-) in sequences")
	scatCmd.Flags().StringP("time-limit", "T", "", "quit after inactive for this time period")
	scatCmd.Flags().IntP("wait-pid", "p", -1, "after process with this PID exited")
	scatCmd.Flags().IntP("delta", "d", 5, "minimum size increase in kilobytes to trigger parsing")
	scatCmd.Flags().StringP("drop-time", "D", "500ms", "Notification drop interval")
	scatCmd.Flags().IntP("qual-ascii-base", "b", 33, "ASCII BASE, 33 for Phred+33")
}
