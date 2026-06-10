// Command replctl is a small CLI for the control plane: register servers,
// create replication jobs, inspect status/RPO, and drive job state. It talks to
// controld over the same authenticated REST API the agent uses.
//
// Configure the endpoint with -control / -token or the CONTROL_URL /
// CONTROL_TOKEN environment variables.
//
// Examples:
//
//	replctl jobs
//	replctl register -name web01 -role source -device /dev/sda
//	replctl create-job -name mig-web01 -target 203.0.113.10:4444 -rpo 60
//	replctl set-state -job 1 -state cutover
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
	"github.com/tiny125/vm-replication/internal/controlclient"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "jobs", "status":
		cmdStatus(args)
	case "servers":
		cmdServers(args)
	case "register":
		cmdRegister(args)
	case "create-job":
		cmdCreateJob(args)
	case "set-state":
		cmdSetState(args)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `replctl — control plane CLI

Commands:
  jobs                       show all jobs with RPO/lag
  servers                    list the server inventory
  register   -name -role -device [-address -hostname]
  create-job -name [-target -source-id -target-id -device -block-size -rpo]
  set-state  -job -state {active|paused|cutover|done|failed}

Global flags (or env CONTROL_URL / CONTROL_TOKEN):
  -control URL    control plane base URL (default $CONTROL_URL)
  -token  TOKEN   bearer token (default $CONTROL_TOKEN)
`)
}

// client builds a control client from common flags, parsing the given flagset.
func client(fs *flag.FlagSet, args []string) *controlclient.Client {
	control := fs.String("control", os.Getenv("CONTROL_URL"), "control plane base URL")
	token := fs.String("token", os.Getenv("CONTROL_TOKEN"), "bearer token")
	_ = fs.Parse(args)
	if *control == "" {
		fmt.Fprintln(os.Stderr, "error: -control or $CONTROL_URL is required")
		os.Exit(2)
	}
	c := controlclient.New(*control, *token)
	return c
}

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func cmdStatus(args []string) {
	fs := flag.NewFlagSet("jobs", flag.ExitOnError)
	c := client(fs, args)
	cx, cancel := ctx()
	defer cancel()
	statuses, err := c.Status(cx)
	fatal(err)
	if len(statuses) == 0 {
		fmt.Println("no jobs")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATE\tRPO\tTARGET\tLAST SYNC\tΔ BLOCKS\tSYNCS")
	for _, st := range statuses {
		rpo := "—"
		if st.LastOKSync != nil {
			rpo = fmt.Sprintf("%.0fs", st.RPOSeconds)
			if st.RPOBreached {
				rpo += " BREACH"
			}
		}
		last := "never"
		delta := "—"
		if st.LastSync != nil {
			last = st.LastSync.FinishedAt.Local().Format("15:04:05") + " " + string(st.LastSync.Mode)
			if !st.LastSync.OK {
				last += " FAILED"
			}
			delta = fmt.Sprintf("%d/%d", st.LastSync.ChangedBlocks, st.LastSync.TotalBlocks)
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%d\n",
			st.Job.ID, st.Job.Name, st.Job.State, rpo, st.Job.TargetAddr, last, delta, st.TotalSyncs)
	}
	tw.Flush()
}

func cmdServers(args []string) {
	fs := flag.NewFlagSet("servers", flag.ExitOnError)
	c := client(fs, args)
	cx, cancel := ctx()
	defer cancel()
	servers, err := c.ListServers(cx)
	fatal(err)
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tROLE\tADDRESS\tDEVICE\tLAST SEEN")
	for _, s := range servers {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.Name, s.Role, s.Address, s.Device, s.LastSeen.Local().Format(time.RFC3339))
	}
	tw.Flush()
}

func cmdRegister(args []string) {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	name := fs.String("name", "", "server name (required)")
	role := fs.String("role", "", "source|target (required)")
	device := fs.String("device", "", "device path")
	address := fs.String("address", "", "host:port or IP")
	hostname := fs.String("hostname", "", "hostname")
	c := client(fs, args)
	if *name == "" || *role == "" {
		fatal(fmt.Errorf("-name and -role are required"))
	}
	cx, cancel := ctx()
	defer cancel()
	sv, err := c.RegisterServer(cx, api.RegisterServerRequest{
		Name: *name, Role: api.Role(*role), Device: *device, Address: *address, Hostname: *hostname,
	})
	fatal(err)
	fmt.Printf("registered server id=%d name=%s role=%s\n", sv.ID, sv.Name, sv.Role)
}

func cmdCreateJob(args []string) {
	fs := flag.NewFlagSet("create-job", flag.ExitOnError)
	name := fs.String("name", "", "job name (required)")
	target := fs.String("target", "", "target receiver host:port")
	srcID := fs.Int64("source-id", 0, "source server id")
	tgtID := fs.Int64("target-id", 0, "target server id")
	device := fs.String("device", "", "device path")
	blockSize := fs.Int("block-size", 0, "block size bytes (0 = agent default)")
	rpo := fs.Int("rpo", 0, "RPO target seconds (0 = unset)")
	c := client(fs, args)
	if *name == "" {
		fatal(fmt.Errorf("-name is required"))
	}
	cx, cancel := ctx()
	defer cancel()
	job, err := c.CreateJob(cx, api.CreateJobRequest{
		Name: *name, TargetAddr: *target, SourceServerID: *srcID, TargetServerID: *tgtID,
		Device: *device, BlockSize: *blockSize, RPOTargetSec: *rpo,
	})
	fatal(err)
	fmt.Printf("created job id=%d name=%s (use -control-job %d on the agent)\n", job.ID, job.Name, job.ID)
}

func cmdSetState(args []string) {
	fs := flag.NewFlagSet("set-state", flag.ExitOnError)
	jobID := fs.Int64("job", 0, "job id (required)")
	state := fs.String("state", "", "active|paused|cutover|done|failed (required)")
	c := client(fs, args)
	if *jobID == 0 || *state == "" {
		fatal(fmt.Errorf("-job and -state are required"))
	}
	cx, cancel := ctx()
	defer cancel()
	job, err := c.SetState(cx, *jobID, api.JobState(*state))
	fatal(err)
	fmt.Printf("job %d (%s) state=%s\n", job.ID, job.Name, job.State)
}
