package simulator

import (
	"context"
	"fmt"
	"net"
	"time"

	amqp091 "github.com/rabbitmq/amqp091-go"
)

// collectorQueue is the queue the collector consumes from, which is what the
// wait for a consumer looks at. It repeats queueName in
// internal/providers/openstack/osloamqp.go, which is unexported and stays so:
// the queue belongs to the collector, and naming it here keeps the collector
// package from exporting anything for the sake of a simulator.
const collectorQueue = "tally-notifications"

// ServiceExchanges are the exchanges the simulator publishes notifications on,
// one per service, and the ones Connect declares. The collector's default
// TALLY_OSC_EXCHANGES lists the first four, and a deployment lists the other
// four itself (docs/openstack-collector.md, "Exchanges and topics"), so a
// collector left at its default receives the month without its load balancers
// and without the keystone, designate, and barbican notifications.
var ServiceExchanges = []string{
	"nova", "cinder", "neutron", "glance", "octavia", "keystone", "designate", "barbican",
}

// probeInterval is how long AwaitConsumer waits between two looks at the
// collector's queue. It is short enough that a collector started a moment later
// is noticed at once, and long enough that a minute of waiting costs the broker
// little.
const probeInterval = 500 * time.Millisecond

// EnsureLocalBroker refuses a broker that is not on the machine the simulator
// runs on.
//
// What a run publishes is well-formed billing data. The exchanges, the declare
// arguments, and the shape of every notification are the ones a real deployment
// carries, and the wire format holds no cloud, so a collector on the far side
// of a production broker books a simulated month as real usage under its own
// TALLY_OSC_CLOUD, whatever TALLY_SIM_CLOUD said. An operator who copies a
// production TALLY_OSC_AMQP_URL into TALLY_SIM_AMQP_URL to try the simulator
// against the real broker writes a month of invented usage into the production
// database: ingestion deduplicates a second run into nothing, and no
// subcommand of tally-reporting-admin deletes what the first one wrote.
//
// A broker on loopback cannot be that one, so it is dialled without asking.
// Every other broker is a decision, which is what --allow-remote-broker is: the
// compose stack passes it because its broker is a container beside the
// simulator, and an operator on a jump host has to type it.
func EnsureLocalBroker(url string) error {
	uri, err := amqp091.ParseURI(url)
	if err != nil {
		// The same answer Connect gives, for the reason it gives there: the parse
		// error carries the whole input, password and all.
		return fmt.Errorf("%s is not a usable AMQP URL", envAMQPURL)
	}
	if uri.Host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(uri.Host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("%s names the broker %s, which is not on this machine: a run publishes "+
		"notifications that a collector books as real usage and nothing deletes again; "+
		"pass --allow-remote-broker when that broker is one to simulate against", envAMQPURL, uri.Host)
}

// Publisher is the simulator's side of the bus: one connection and one channel
// in confirm mode, so every published notification is waited on until the
// broker has taken responsibility for it. A run that reported a message
// published while the broker dropped it would leave the collector short of
// events with nothing to point at.
type Publisher struct {
	conn    *amqp091.Connection
	channel *amqp091.Channel
}

// Connect dials the broker, opens the confirming channel, and declares the
// service exchanges.
//
// The declares are what make the collector's connection work at all: the
// collector declares the same exchanges passively (connect in
// internal/providers/openstack/osloamqp.go), so on a fresh broker, where no
// OpenStack service has ever published, its declare fails and it reconnects
// until somebody declares them. The arguments are the ones the services use,
// and the ones declareExchanges in
// internal/providers/openstack/amqp_integration_test.go uses: a durable topic
// exchange that is neither auto-deleted nor internal. A declare that differed
// in any of them would be refused by a broker that already carries the
// service's exchange.
func Connect(url string) (*Publisher, error) {
	// The parse error is thrown away rather than wrapped, the way connect in
	// internal/providers/openstack/osloamqp.go throws it away: net/url formats
	// the whole input into that one error, password and all, and the simulator
	// writes what it returns to the log. What Dial reports afterwards carries the
	// host and the port alone.
	if _, err := amqp091.ParseURI(url); err != nil {
		return nil, fmt.Errorf("%s is not a usable AMQP URL", envAMQPURL)
	}
	conn, err := amqp091.Dial(url)
	if err != nil {
		return nil, fmt.Errorf("dialing the broker: %w", err)
	}
	channel, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("opening a channel: %w", err)
	}
	if err := channel.Confirm(false); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("enabling publisher confirms: %w", err)
	}
	for _, exchange := range ServiceExchanges {
		if err := channel.ExchangeDeclare(exchange, "topic",
			true, false, false, false, nil); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("declaring the exchange %s: %w", exchange, err)
		}
	}
	return &Publisher{conn: conn, channel: channel}, nil
}

// AwaitConsumer waits until queue has at least one consumer, or until timeout
// has passed. A timeout of zero skips the wait.
//
// The wait is what keeps a run from publishing into nothing: a topic exchange
// drops every message no queue is bound to, and on a fresh broker the
// collector's queue does not exist until the collector has connected once and
// bound it. The consumer count rather than the queue's existence is the signal,
// because the collector declares, binds, and only then registers its consumer:
// a queue that has one is a collector that has finished connecting.
func (p *Publisher) AwaitConsumer(ctx context.Context, queue string, timeout time.Duration) error {
	if timeout == 0 {
		return nil
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// Every probe takes a channel of its own because a passive declare of a
		// queue that does not exist closes the channel it ran on with a 404, and
		// the publishing channel has to survive the wait.
		channel, err := p.conn.Channel()
		if err != nil {
			return fmt.Errorf("opening a channel: %w", err)
		}
		q, err := channel.QueueDeclarePassive(queue, true, false, false, false, nil)
		if err == nil {
			_ = channel.Close()
			if q.Consumers >= 1 {
				return nil
			}
		}

		timer := time.NewTimer(probeInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("no consumer on the queue %s appeared within %s; "+
		"start the collector first, or pass --wait-for-collector 0 to publish anyway", queue, timeout)
}

// Publish puts one notification on an exchange and returns once the broker has
// confirmed it. The confirm is waited on per message rather than in batches,
// which is what lets the run report the count it published and stop at the
// first message the broker refused.
//
// The message is persistent because the collector's queue is durable: a backlog
// that piled up there while the collector was stopped is meant to survive a
// broker restart, and a transient message would be gone with it.
func (p *Publisher) Publish(ctx context.Context, exchange, routingKey string, body []byte) error {
	confirmation, err := p.channel.PublishWithDeferredConfirmWithContext(ctx, exchange, routingKey,
		false, false, amqp091.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp091.Persistent,
			Body:         body,
		})
	if err != nil {
		return fmt.Errorf("publishing to %s: %w", exchange, err)
	}
	ok, err := confirmation.WaitContext(ctx)
	if err != nil {
		return fmt.Errorf("publishing to %s: %w", exchange, err)
	}
	if !ok {
		return fmt.Errorf("the broker did not confirm a message on %s", exchange)
	}
	return nil
}

// Close ends the connection to the broker, and the channel with it.
func (p *Publisher) Close() error {
	if err := p.conn.Close(); err != nil {
		return fmt.Errorf("closing the broker connection: %w", err)
	}
	return nil
}
