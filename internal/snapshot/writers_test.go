package snapshot

import (
	"strings"
	"testing"
)

// sampleFuser is a representative `fuser -vm /` table: the header, the pseudo
// "kernel mount" row, a run of kernel threads, then the userspace processes an
// operator could actually stop.
const sampleFuser = `                     USER        PID ACCESS COMMAND
/:                   root     kernel mount /
                     root          1 frce. systemd
                     root          2 .rc.. kthreadd
                     root          3 .rc.. pool_workqueue_release
                     root          9 .rc.. kworker/0:0-events
                     root         16 .rc.. ksoftirqd/0
                     root        892 F.... nginx
                     www-data    893 F.... nginx
                     root       1201 Frce. sshd
                     postgres   1450 F.... postgres`

// kernel threads in the sample, by pid.
func sampleIsKernel(pid int) bool {
	switch pid {
	case 2, 3, 9, 16:
		return true
	}
	return false
}

// The message exists to tell an operator which of THEIR processes to stop. A
// kernel thread can never be stopped, so listing them is pure noise — and worse
// than noise here, because fuser lists in pid order and kernel threads have the
// low pids, so a length cap keeps them and discards the userspace processes that
// are the entire point of the message.
func TestSummarizeWritersKeepsOnlyActionableProcesses(t *testing.T) {
	got := summarizeWriters(sampleFuser, sampleIsKernel)

	for _, want := range []string{"nginx", "sshd", "postgres", "systemd"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary must name the userspace process %q; got %q", want, got)
		}
	}
	for _, unwanted := range []string{"kthreadd", "kworker", "ksoftirqd", "pool_workqueue_release"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("summary must not mention the kernel thread %q (an operator cannot stop it); got %q", unwanted, got)
		}
	}
	// The table scaffolding must not survive either.
	for _, noise := range []string{"USER", "ACCESS", "COMMAND", "kernel mount"} {
		if strings.Contains(got, noise) {
			t.Errorf("summary must not carry the fuser table scaffolding %q; got %q", noise, got)
		}
	}
}

// Two nginx workers are one thing to stop, not two lines of output.
func TestSummarizeWritersGroupsByCommand(t *testing.T) {
	got := summarizeWriters(sampleFuser, sampleIsKernel)
	if n := strings.Count(got, "nginx"); n != 1 {
		t.Errorf("nginx has two pids but should be named once, got %d occurrences in %q", n, got)
	}
	if !strings.Contains(got, "892") || !strings.Contains(got, "893") {
		t.Errorf("both nginx pids should still be shown; got %q", got)
	}
}

// The old implementation flattened the whole table and truncated at 500 chars.
// The summary has to stay short enough to read in an activity-log line.
func TestSummarizeWritersStaysShort(t *testing.T) {
	var b strings.Builder
	b.WriteString("                     USER        PID ACCESS COMMAND\n")
	b.WriteString("/:                   root     kernel mount /\n")
	for i := 100; i < 400; i++ {
		b.WriteString("                     root       ")
		b.WriteString(itoa(i))
		b.WriteString(" F.... daemon")
		b.WriteString(itoa(i))
		b.WriteString("\n")
	}
	got := summarizeWriters(b.String(), func(int) bool { return false })
	if len(got) > 300 {
		t.Errorf("summary must stay readable in one log line, got %d chars: %q", len(got), got)
	}
	if !strings.Contains(got, "more") {
		t.Errorf("a truncated summary should say how many more were omitted; got %q", got)
	}
}

// When only kernel threads hold the mount there is nothing actionable to say,
// and the caller must be able to tell that apart from "we could not look".
func TestSummarizeWritersEmptyWhenOnlyKernelThreads(t *testing.T) {
	only := `                     USER        PID ACCESS COMMAND
/:                   root     kernel mount /
                     root          2 .rc.. kthreadd
                     root          9 .rc.. kworker/0:0-events`
	if got := summarizeWriters(only, sampleIsKernel); got != "" {
		t.Errorf("a kernel-thread-only table has nothing actionable to report, got %q", got)
	}
	if got := summarizeWriters("", sampleIsKernel); got != "" {
		t.Errorf("empty fuser output must summarize to empty, got %q", got)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
