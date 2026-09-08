package opencode

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// opencode keeps its database in WAL mode and writes to it continuously while a
// session runs. Everything this package does is a read-only read of that live
// file, so it has to see rows that are still in the write-ahead log and have not
// been checkpointed into the main database yet. If it didn't, a transcript would
// silently freeze at the last checkpoint — which looks exactly like "the agent
// stopped talking".
func TestReadsUncheckpointedWALWrites(t *testing.T) {
	s := newTestStore(t)
	run(t, s.sqlite, s.dbPath, "pragma journal_mode=wal;")
	addSession(t, s, "ses_wal001", "/work", "WAL session", "", 100)
	addMessage(t, s, "ses_wal001", "msg_1", "user", 10, textPart("before the writer opened"))

	// Hold a connection open for the whole test. SQLite checkpoints the WAL when
	// the last connection closes, so without this the write below would be
	// folded into the database and the test would prove nothing.
	holder := exec.Command(s.sqlite, s.dbPath)
	stdin, err := holder.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = holder.Wait()
	}()
	if _, err := io.WriteString(stdin, "select count(*) from message;\n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	// A separate process appends, as opencode does.
	run(t, s.sqlite, s.dbPath, fmt.Sprintf(
		"insert into message (id, session_id, time_created, time_updated, data)"+
			" values ('msg_2', 'ses_wal001', 20, 20, '{\"role\":\"assistant\"}');"+
			"insert into part (id, message_id, session_id, time_created, time_updated, data)"+
			" values ('msg_2-p0', 'msg_2', 'ses_wal001', 21, 21, %s);", quote(textPart("written into the WAL"))))

	// Guard against a vacuous pass: if the holder connection above failed to
	// keep the WAL alive, the write was checkpointed into the database and this
	// test would be reading an ordinary file while claiming otherwise.
	walInfo, err := os.Stat(s.dbPath + "-wal")
	if err != nil || walInfo.Size() == 0 {
		t.Fatalf("no live WAL to read from (err=%v) — this test is not exercising what it claims", err)
	}

	got, err := s.Render("ses_wal001", 100)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(got, "written into the WAL") {
		t.Fatalf("a read-only read missed an uncheckpointed write — transcripts would freeze:\n%s", got)
	}
	// And the fingerprint must move, or the client would be told nothing changed.
	fingerprint, err := s.Fingerprint("ses_wal001", 100)
	if err != nil {
		t.Fatal(err)
	}
	run(t, s.sqlite, s.dbPath,
		"insert into part (id, message_id, session_id, time_created, time_updated, data)"+
			" values ('msg_2-p1', 'msg_2', 'ses_wal001', 22, 22, '{\"type\":\"text\",\"text\":\"more\"}');")
	after, err := s.Fingerprint("ses_wal001", 100)
	if err != nil {
		t.Fatal(err)
	}
	if after == fingerprint {
		t.Error("fingerprint did not move for a WAL-resident append")
	}
}
