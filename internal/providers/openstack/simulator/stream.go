package simulator

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/b42labs/tally/internal/providers/openstack"
)

// Line is one line of notifications.jsonl: the message body a service put on
// the bus, together with the addressing it was published under. The addressing
// travels with the body because the body does not carry it: an exchange is
// what decides which queue a notification reaches, and a replay that guessed it
// would publish a month no collector receives.
type Line struct {
	// Exchange is the service exchange the notification belongs on, one of nova,
	// cinder, neutron, and glance.
	Exchange string `json:"exchange"`
	// RoutingKey is the topic the notification was published under.
	RoutingKey string `json:"routing_key"`
	// Body is the oslo envelope as Render produced it, kept as raw JSON so a
	// replay republishes the very bytes the run generated rather than a
	// re-encoding of them.
	Body json.RawMessage `json:"body"`
}

// WriteStream writes the schedule to path as one notification per line,
// creating the file or truncating what is already there.
//
// It is what lets a generated month outlive the process that generated it: the
// file holds every notification with the exchange and the routing key it was
// meant for, so replay puts the same month on a broker again without the
// generator, and without the seed that produced it.
func WriteStream(path string, schedule Schedule) (err error) {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	// The close is what reports the write the file system deferred, such as a
	// full disk, so its error is not dropped. An error already on its way says
	// more about what went wrong and keeps its place.
	defer func() {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("writing %s: %w", path, closeErr)
		}
	}()

	writer := bufio.NewWriter(file)
	encoder := json.NewEncoder(writer)
	for _, transition := range schedule {
		body, renderErr := Render(transition)
		if renderErr != nil {
			return renderErr
		}
		if encodeErr := encoder.Encode(Line{
			Exchange:   transition.Exchange,
			RoutingKey: routingKey,
			Body:       body,
		}); encodeErr != nil {
			return fmt.Errorf("writing %s: %w", path, encodeErr)
		}
	}

	if err := writer.Flush(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// WriteEvents writes the events the collector is expected to record for the
// schedule to path, one per line, creating the file or truncating what is
// already there. The cloud is the one the events are booked to.
//
// It states what a run expects of the collector. notifications.jsonl says what
// was published, this file says what ingestion has to hold once the month is
// consumed, and a drill compares the two without mapping the notifications a
// second time by hand.
//
// A transition the schedule marks billable and the mapping claims nothing for
// fails the write. The workload decides billable from its own catalog and the
// collector decides it from its mapping table, so a type added to one and not
// the other drifts the two apart. Failing here is what catches that drift
// outside the test suite, on whatever month an operator generates, instead of
// writing a file that quietly expects fewer events than the month holds.
func WriteEvents(path, cloud string, schedule Schedule) (err error) {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	defer func() {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("writing %s: %w", path, closeErr)
		}
	}()

	writer := bufio.NewWriter(file)
	encoder := json.NewEncoder(writer)
	for _, transition := range schedule.Billable() {
		body, renderErr := Render(transition)
		if renderErr != nil {
			return renderErr
		}
		// The event comes from the collector's own decoder and mapping rather than
		// from the transition, so this file holds what the collector produces and
		// not what the simulator believes it produces.
		notification, parseErr := openstack.ParseEnvelope(body)
		if parseErr != nil {
			return fmt.Errorf("writing %s: %w", path, parseErr)
		}
		mapped, ok := openstack.MapNotification(notification, cloud)
		if !ok {
			return fmt.Errorf("transition %s at %s is marked billable but the mapping claims nothing for it",
				transition.EventType, transition.At.UTC().Format(time.RFC3339))
		}

		if encodeErr := encoder.Encode(mapped); encodeErr != nil {
			return fmt.Errorf("writing %s: %w", path, encodeErr)
		}
	}

	if err := writer.Flush(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// ReadStream reads the notifications of a stream file back, in the order they
// stand in the file, which is the order they were published in.
//
// Every body goes through the collector's own envelope parser here rather than
// at publication time. A file that holds something other than notifications, or
// a notification without a usable timestamp, is refused before the first
// message reaches the broker, instead of halfway through a replay that has
// already published part of a month.
//
// The routing key of a line is read from the line and not assumed. A month
// recorded against one deployment may have been published under a topic this
// build no longer defaults to, and a replay has to reach the queue the
// recording did.
func ReadStream(path string) ([]Line, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	// An instance create renders past what bufio.Scanner reads with by default,
	// so the limit is raised to a megabyte. A longer line is a file this package
	// did not write, and it is reported rather than silently cut in half.
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)

	var lines []Line
	for number := 1; scanner.Scan(); number++ {
		var line Line
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			return nil, fmt.Errorf("%s: line %d: %w", path, number, err)
		}
		if _, err := openstack.ParseEnvelope(line.Body); err != nil {
			return nil, fmt.Errorf("%s: line %d: %w", path, number, err)
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("%s holds no notifications", path)
	}
	return lines, nil
}
