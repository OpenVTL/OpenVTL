package inventory

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// JournalEvent is a classified mhVTL daemon log line.
type JournalEvent struct {
	TS      time.Time
	Kind    string // write | read | load | unload | move | filemarks | pr
	QueueID int    // vtltape@NN instance (0 for vtllibrary)
	Detail  string
}

// Classification rules. mhVTL logs at VERBOSE=3 are firehose-grade;
// write/read matches are counted and aggregated by the engine rather
// than forwarded per line. Patterns tuned against live mhVTL 1.8.0 journals.
var journalRules = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"write", regexp.MustCompile(`ssc_write_6`)},
	{"read", regexp.MustCompile(`ssc_read_6`)},
	{"load", regexp.MustCompile(`TAPE LOADING|Loading media|Tape loaded`)},
	{"unload", regexp.MustCompile(`TAPE UNLOADED|Unloading|ssc_load_unload.*UNLOAD`)},
	{"move", regexp.MustCompile(`smc_move_medium|MOVE MEDIUM`)},
	{"filemarks", regexp.MustCompile(`[Ww]rite.?[Ff]ilemarks|WRITE FILEMARKS`)},
	{"pr", regexp.MustCompile(`PERSISTENT RESERVE (IN|OUT)`)},
}

var reUnitQueue = regexp.MustCompile(`vtltape@(\d+)\.service`)

// TailJournal follows vtltape/vtllibrary units and emits classified
// events until ctx is cancelled. Uses journalctl -f -o json (keeps the
// binary CGO-free vs. sdjournal).
func TailJournal(ctx context.Context, since string, out chan<- JournalEvent, log *slog.Logger) error {
	args := []string{"-f", "-o", "json", "--no-pager",
		"-t", "vtltape", "-t", "vtllibrary"}
	if since != "" {
		args = append(args, "--since", since)
	}
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer cmd.Wait()

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var rec struct {
			Message string `json:"MESSAGE"`
			Unit    string `json:"_SYSTEMD_UNIT"`
			RT      string `json:"__REALTIME_TIMESTAMP"`
		}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		for _, rule := range journalRules {
			if !rule.re.MatchString(rec.Message) {
				continue
			}
			ev := JournalEvent{Kind: rule.kind, Detail: strings.TrimSpace(rec.Message)}
			if usec, err := strconv.ParseInt(rec.RT, 10, 64); err == nil {
				ev.TS = time.UnixMicro(usec)
			} else {
				ev.TS = time.Now()
			}
			if m := reUnitQueue.FindStringSubmatch(rec.Unit); m != nil {
				ev.QueueID, _ = strconv.Atoi(m[1])
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return ctx.Err()
			}
			break // first matching rule wins
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	log.Warn("journal tail ended unexpectedly", "err", sc.Err())
	return sc.Err()
}
