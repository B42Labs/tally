// Command tally-openstack-simulator publishes one simulated month of OpenStack
// notifications.
//
// The run subcommand generates the month from --seed and --period and publishes
// it onto the broker TALLY_SIM_AMQP_URL names, at --factor virtual seconds per
// wall second. With --out it writes the month to notifications.jsonl,
// events.jsonl and oracle.json instead, and with both it does both. With
// --faults it turns fault switches on, each of which changes what the bus
// carries and never the month the oracle states; the run help names the six.
// The replay subcommand publishes a notifications.jsonl an earlier run wrote,
// which puts the same month on a bus again without the generator behind it. The
// compare subcommand reads the oracle a run wrote, an engine export of the
// month and the pricing model the run rated with, and lists every resource
// whose metered intervals or quantities differ from the oracle; it exits 1 when
// anything differs.
//
// The control endpoint on TALLY_SIM_HTTP_PORT changes the factor while a run
// publishes, so a month that is going out too slowly is sped up without being
// started over. POST /release on it publishes the notifications a run with the
// held-back fault switch kept back.
//
// SIGINT and SIGTERM stop a run cleanly: what went out stays out, and the
// process ends with exit status 0. Every other failure exits 1.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

func main() {
	// The tree runs on a context the process's own termination cancels, which is
	// what a run needs: it spends its whole life inside the publishing loop, and
	// cancellation is what turns a signal into a stop that keeps the messages
	// already published rather than a process killed mid-month.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Cobra has already written the error to stderr by the time Execute
	// returns it.
	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}

// newRootCmd assembles the command tree. Nothing in it reads the process's
// arguments or streams, so a test drives the same tree through SetArgs, SetOut,
// and SetErr.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "tally-openstack-simulator",
		Short: "Publish a simulated month of OpenStack notifications",
		// A broker that is down is not a usage mistake, and dumping the help text
		// after one buries the line that says what went wrong.
		SilenceUsage: true,
	}

	root.AddCommand(
		newRunCmd(),
		newReplayCmd(),
		newCompareCmd(),
	)
	return root
}
