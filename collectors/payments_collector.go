package collectors

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	// totalPayments tracks the total number of payments initiated, labeled
	// by final payment status. This permits computation of both throughput
	// and success/failure rates.
	totalPayments = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "lnd_total_payments",
			Help: "Total number of payments initiated, labeled by final status",
		},
		[]string{"status"},
	)

	// totalHTLCAttempts tracks the number of HTLC attempts made based on
	// the payment status (success or fail). When combined with the payment
	// counter, this permits tracking the number of attempts per payment.
	totalHTLCAttempts = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "lnd_total_htlc_attempts",
			Help: "Total number of HTLC attempts across all payments, labeled by final payment status",
		},
		[]string{"status"},
	)

	// paymentAttempts is a histogram for visualizing what portion of
	// payments complete within a given number of attempts.
	paymentAttempts = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "lnd_payment_attempts_per_payment",
			Help:    "Histogram tracking the number of attempts per payment",
			Buckets: prometheus.ExponentialBuckets(1, 2, 10),
		},
	)

	// totalPaymentsSat tracks the total volume of payments sent in
	// satoshis, labeled by final payment status. This permits computation
	// of both successfully sent volume and volume of failed send
	// attempts.
	totalPaymentsSat = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "lnd_total_payments_sat",
			Help: "Total volume of payments sent in satoshis, labeled by final status",
		},
		[]string{"status"},
	)

	// totalPaymentsFeesSat tracks the total routing fees paid for
	// payments sent, in satoshis, labeled by final payment status. Since
	// failed payments pay no routing fee, in practice only the
	// "succeeded" label accumulates a non-zero total.
	totalPaymentsFeesSat = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "lnd_total_payments_fees_sat",
			Help: "Total routing fees paid for payments sent, in satoshis, labeled by final status",
		},
		[]string{"status"},
	)

	// paymentDuration is a histogram tracking how long it took a payment
	// to reach a terminal state, measured from creation to the
	// resolution of its terminal HTLC attempt. This allows median/avg
	// payment speed to be computed via histogram_quantile and
	// sum/count respectively.
	paymentDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "lnd_payment_duration_seconds",
			Help: "Histogram tracking the time (in seconds) taken " +
				"for a payment to reach a terminal state, " +
				"labeled by final status",
			Buckets: []float64{
				0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600,
			},
		},
		[]string{"status"},
	)

	// paymentHops is a histogram tracking the number of hops in the
	// route used by a payment's terminal HTLC attempt. This allows
	// median/avg hop count to be computed via histogram_quantile and
	// sum/count respectively.
	paymentHops = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "lnd_payment_num_hops",
			Help: "Histogram tracking the number of hops in the " +
				"route of a payment's terminal HTLC attempt, " +
				"labeled by final status",
			Buckets: prometheus.LinearBuckets(1, 1, 20),
		},
		[]string{"status"},
	)
)

// paymentsMonitor listens for payments and updates Prometheus metrics.
type paymentsMonitor struct {
	client routerrpc.RouterClient

	lnd *lndclient.LndServices

	errChan chan error

	// quit is closed to signal that we need to shutdown.
	quit chan struct{}

	wg sync.WaitGroup
}

// newPaymentsMonitor creates a new payments monitor and ensures the context
// includes macaroon authentication.
func newPaymentsMonitor(lnd *lndclient.LndServices,
	errChan chan error) *paymentsMonitor {

	return &paymentsMonitor{
		client:  routerrpc.NewRouterClient(lnd.ClientConn),
		lnd:     lnd,
		errChan: errChan,
		quit:    make(chan struct{}),
	}
}

// start subscribes to `TrackPayments` and updates Prometheus metrics.
func (p *paymentsMonitor) start() error {
	paymentLogger.Info("Starting payments monitor...")

	// Attach macaroon authentication for the router service.
	ctx, cancel := context.WithCancel(context.Background())
	ctx, err := p.lnd.WithMacaroonAuthForService(
		ctx, lndclient.RouterServiceMac,
	)
	if err != nil {
		cancel()

		return fmt.Errorf("failed to get macaroon-authenticated "+
			"context: %w", err)
	}

	stream, err := p.client.TrackPayments(
		ctx, &routerrpc.TrackPaymentsRequest{
			// NOTE: We only need to know the final result of the
			// payment and all attempts.
			NoInflightUpdates: true,
		},
	)
	if err != nil {
		paymentLogger.Errorf("Failed to subscribe to TrackPayments: %v",
			err)

		cancel()

		return err
	}

	p.wg.Add(1)
	go func() {
		defer func() {
			cancel()
			p.wg.Done()
		}()

		for {
			select {
			case <-p.quit:
				return

			default:
				payment, err := stream.Recv()
				if err != nil {
					paymentLogger.Errorf("Error receiving "+
						"payment update: %v", err)

					p.errChan <- err
					return
				}
				processPaymentUpdate(payment)
			}
		}
	}()

	return nil
}

// stop cancels the payments monitor subscription.
func (p *paymentsMonitor) stop() {
	paymentLogger.Info("Stopping payments monitor...")

	close(p.quit)
	p.wg.Wait()
}

// collectors returns all of the collectors that the htlc monitor uses.
func (p *paymentsMonitor) collectors() []prometheus.Collector {
	return []prometheus.Collector{
		totalPayments, totalHTLCAttempts, paymentAttempts,
		totalPaymentsSat, totalPaymentsFeesSat, paymentDuration,
		paymentHops,
	}
}

// processPaymentUpdate updates Prometheus metrics based on received payments.
//
// NOTE: It is expected that this receive the *final* payment update with the
// complete list of all htlc attempts made for this payment.
func processPaymentUpdate(payment *lnrpc.Payment) {
	var status string

	switch payment.Status {
	case lnrpc.Payment_SUCCEEDED:
		status = "succeeded"
	case lnrpc.Payment_FAILED:
		status = "failed"
	default:
		// We don't expect this given that this should be a terminal
		// payment update.
		status = "unknown"
	}

	// Increment metrics with proper label.
	totalPayments.WithLabelValues(status).Inc()
	totalPaymentsSat.WithLabelValues(status).Add(float64(payment.ValueSat))
	totalPaymentsFeesSat.WithLabelValues(status).Add(float64(payment.FeeSat))

	attemptCount := len(payment.Htlcs)
	totalHTLCAttempts.WithLabelValues(status).Add(float64(attemptCount))

	paymentAttempts.Observe(float64(attemptCount))

	// Record the duration and hop count of the HTLC attempt that
	// resolved the payment: the successful attempt if the payment
	// succeeded, or otherwise the most recent attempt that led to the
	// payment's final failure.
	if htlc := terminalHTLCAttempt(payment.Htlcs); htlc != nil {
		if payment.CreationTimeNs > 0 && htlc.ResolveTimeNs > 0 {
			durationNs := htlc.ResolveTimeNs - payment.CreationTimeNs
			if durationNs > 0 {
				paymentDuration.WithLabelValues(status).Observe(
					time.Duration(durationNs).Seconds(),
				)
			}
		}

		if htlc.Route != nil {
			paymentHops.WithLabelValues(status).Observe(
				float64(len(htlc.Route.Hops)),
			)
		}
	}

	paymentLogger.Debugf("Payment %s updated: status=%s, %d attempts",
		payment.PaymentHash, status, attemptCount)
}

// terminalHTLCAttempt returns the HTLC attempt that determined the payment's
// final outcome: the successful attempt, if any, or else the most recently
// attempted HTLC (the one responsible for the payment's final failure).
func terminalHTLCAttempt(htlcs []*lnrpc.HTLCAttempt) *lnrpc.HTLCAttempt {
	var lastAttempt *lnrpc.HTLCAttempt

	for _, htlc := range htlcs {
		if htlc.Status == lnrpc.HTLCAttempt_SUCCEEDED {
			return htlc
		}

		if lastAttempt == nil ||
			htlc.AttemptTimeNs > lastAttempt.AttemptTimeNs {

			lastAttempt = htlc
		}
	}

	return lastAttempt
}
