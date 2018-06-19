package fixchain

import (
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/certificate-transparency-go/x509"
)

// Fixer contains methods to fix certificate chains and properties to store
// information about each attempt that is made to fix a certificate chain.
type Fixer struct {
	toFix  chan *toFix
	chains chan []*x509.Certificate // Chains successfully fixed by the fixer
	active uint32
	// Counters may not be entirely accurate due to non-atomicity
	reconstructed    uint
	notReconstructed uint
	fixed            uint
	notFixed         uint
	skipped          uint
	alreadyDone      uint

	wg     sync.WaitGroup
	errors chan *FixError
	cache  *urlCache
	done   *lockedMap
}

// QueueChain adds the given cert and chain to the queue to be fixed by the
// fixer, with respect to the given roots
func (f *Fixer) QueueChain(cert *x509.Certificate, d *DedupedChain, roots *x509.CertPool) {
	intermediates := x509.NewCertPool()
	for _, c := range d.certs {
		intermediates.AddCert(c)
	}

	opts := x509.VerifyOptions{
		Intermediates:     intermediates,
		Roots:             roots,
		DisableTimeChecks: true,
		KeyUsages:         []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}

	f.toFix <- &toFix{cert: cert, chain: d, opts: &opts, fixer: f}
}

// Wait for all the fixers to finish
func (f *Fixer) Wait() {
	close(f.toFix)
	f.wg.Wait()
}

func (f *Fixer) fixServer() {
	defer f.wg.Done()

	for fix := range f.toFix {
		atomic.AddUint32(&f.active, 1)
		chains, ferr := fix.handleChain()
		if ferr != nil {
			f.errors <- ferr
		} else {
			for _, chain := range chains {
				f.chains <- chain
			}
		}
		atomic.AddUint32(&f.active, ^uint32(0))
	}
}

func (f *Fixer) newFixServerPool() {
	for i := 0; i < 100; i++ {
		f.wg.Add(1)
		go f.fixServer()
	}
}

func (f *Fixer) logStats() {
	t := time.NewTicker(time.Second)
	go func() {
		for _ = range t.C {
			log.Printf("fixers: %d active, "+
				"%d reconstructed, %d not reconstructed, "+
				"%d fixed, %d not fixed, %d skipped, %d already done",
				f.active, f.reconstructed, f.notReconstructed,
				f.fixed, f.notFixed, f.skipped, f.alreadyDone)
		}
	}()
}

// NewFixer creates a new fixer and starts up a pool of workers.  Errors are
// pushed to the errors channel, and fixed chains are pushed to the chains
// channel.
func NewFixer(chains chan []*x509.Certificate, errors chan *FixError, client *http.Client) *Fixer {
	f := &Fixer{
		toFix:  make(chan *toFix),
		chains: chains,
		errors: errors,
		cache:  newURLCache(client),
		done:   newLockedMap(),
	}

	f.newFixServerPool()
	f.logStats()
	return f
}
