package collectors

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	invoiceStateLabel = "state"

	invoiceStateOpenValue     = "open"
	invoiceStateSettledValue  = "settled"
	invoiceStateCanceledValue = "canceled"
	invoiceStateAcceptedValue = "accepted"
	invoiceStateUnknownValue  = "unknown"
)

var (
	// totalInvoices tracks the total number of invoice updates received,
	// labeled by invoice state. Since an invoice only ever settles or is
	// canceled once, the "settled" and "canceled" label values give an
	// accurate count of completed invoices, while "open" and "accepted"
	// track invoices as they move through their lifecycle.
	totalInvoices = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "lnd_total_invoices",
			Help: "Total number of invoice updates received, labeled by invoice state",
		},
		[]string{invoiceStateLabel},
	)

	// totalInvoicesSat tracks the volume of invoices in satoshis, labeled
	// by invoice state. For the "settled" state this is the amount
	// actually paid to us; for all other states it is the requested
	// invoice amount.
	totalInvoicesSat = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "lnd_total_invoices_sat",
			Help: "Total volume of invoices in satoshis, labeled by invoice state",
		},
		[]string{invoiceStateLabel},
	)
)

// invoicesMonitor listens for invoice updates and updates Prometheus
// metrics.
type invoicesMonitor struct {
	lnd *lndclient.LndServices

	errChan chan error

	// quit is closed to signal that we need to shutdown.
	quit chan struct{}

	wg sync.WaitGroup
}

// newInvoicesMonitor creates a new invoices monitor.
func newInvoicesMonitor(lnd *lndclient.LndServices,
	errChan chan error) *invoicesMonitor {

	return &invoicesMonitor{
		lnd:     lnd,
		errChan: errChan,
		quit:    make(chan struct{}),
	}
}

// start subscribes to invoice updates and updates Prometheus metrics.
func (i *invoicesMonitor) start() error {
	invoiceLogger.Info("Starting invoices monitor...")

	ctx, cancel := context.WithCancel(context.Background())

	invoiceUpdates, streamErrChan, err := i.lnd.Client.SubscribeInvoices(
		ctx, lndclient.InvoiceSubscriptionRequest{},
	)
	if err != nil {
		cancel()

		return fmt.Errorf("failed to subscribe to invoices: %w", err)
	}

	i.wg.Add(1)
	go func() {
		defer func() {
			cancel()
			i.wg.Done()
		}()

		for {
			select {
			case <-i.quit:
				return

			case invoice, ok := <-invoiceUpdates:
				if !ok {
					i.errChan <- errors.New("invoice " +
						"update stream terminated")
					return
				}
				processInvoiceUpdate(invoice)

			case err, ok := <-streamErrChan:
				i.errChan <- fmt.Errorf("invoice stream "+
					"exited: %v, closed: %v", err, ok)
				return
			}
		}
	}()

	return nil
}

// stop cancels the invoices monitor subscription.
func (i *invoicesMonitor) stop() {
	invoiceLogger.Info("Stopping invoices monitor...")

	close(i.quit)
	i.wg.Wait()
}

// collectors returns all of the collectors that the invoices monitor uses.
func (i *invoicesMonitor) collectors() []prometheus.Collector {
	return []prometheus.Collector{
		totalInvoices, totalInvoicesSat,
	}
}

// processInvoiceUpdate updates Prometheus metrics based on a received
// invoice update.
func processInvoiceUpdate(invoice *lndclient.Invoice) {
	var (
		state string
		amt   int64
	)

	switch invoice.State {
	case invoices.ContractOpen:
		state = invoiceStateOpenValue
		amt = int64(invoice.Amount.ToSatoshis())

	case invoices.ContractAccepted:
		state = invoiceStateAcceptedValue
		amt = int64(invoice.Amount.ToSatoshis())

	case invoices.ContractCanceled:
		state = invoiceStateCanceledValue
		amt = int64(invoice.Amount.ToSatoshis())

	case invoices.ContractSettled:
		state = invoiceStateSettledValue
		amt = int64(invoice.AmountPaid.ToSatoshis())

	default:
		state = invoiceStateUnknownValue
	}

	totalInvoices.WithLabelValues(state).Inc()
	totalInvoicesSat.WithLabelValues(state).Add(float64(amt))

	invoiceLogger.Debugf("Invoice %v updated: state=%s, amount=%d sat",
		invoice.Hash, state, amt)
}
