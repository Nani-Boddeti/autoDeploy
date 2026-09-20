package githubapp

import (
	"errors"
	"os"
)

// AskPassBroker supplies exactly one password via an anonymous pipe. Its
// descriptor number is non-secret; the token itself is never an argument or
// environment value.
type AskPassBroker struct {
	reader *os.File
	writer *os.File
}

// NewAskPassBrokerFromToken builds a broker around a checkout token
// presented as a plain value, rather than one this process minted itself.
// It exists so an agent-side gitcheckout.Lease implementation (which fetches
// its token from POST /agent/v1/checkout/start, over the authenticated
// channel ADR 0003 permits it to cross) can feed Git the same one-use pipe
// discipline CheckoutLease.NewAskPassBroker uses, without the agent ever
// gaining any capability to mint or revoke a token itself: this constructor
// wraps an already-issued value and nothing else. It never touches a
// Provider, a Signer, or an App private key.
func NewAskPassBrokerFromToken(token string) (*AskPassBroker, error) {
	return newAskPassBroker(token)
}

func newAskPassBroker(token string) (*AskPassBroker, error) {
	if token == "" {
		return nil, errors.New("empty checkout token")
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	if _, err = w.WriteString(token + "\n"); err != nil {
		r.Close()
		w.Close()
		return nil, err
	}
	if err = w.Close(); err != nil {
		r.Close()
		return nil, err
	}
	return &AskPassBroker{reader: r}, nil
}

// Reader is passed as an inherited extra file. Ownership transfers to the
// command runner, which must close it after the Git process exits.
func (b *AskPassBroker) Reader() *os.File {
	if b == nil {
		return nil
	}
	r := b.reader
	b.reader = nil
	return r
}

func (b *AskPassBroker) Close() error {
	if b == nil || b.reader == nil {
		return nil
	}
	err := b.reader.Close()
	b.reader = nil
	return err
}
