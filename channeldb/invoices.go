package channeldb

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// invoiceBucket is the name of the bucket within the database that
	// stores all data related to invoices no matter their final state.
	// Within the invoice bucket, each invoice is keyed by its invoice ID
	// which is a monotonically increasing uint32.
	invoiceBucket = []byte("invoices")

	// paymentHashIndexBucket is the name of the sub-bucket within the
	// invoiceBucket which indexes all invoices by their payment hash. The
	// payment hash is the sha256 of the invoice's payment preimage. This
	// index is used to detect duplicates, and also to provide a fast path
	// for looking up incoming HTLCs to determine if we're able to settle
	// them fully.
	//
	// maps: payHash => invoiceKey
	invoiceIndexBucket = []byte("paymenthashes")

	// numInvoicesKey is the name of key which houses the auto-incrementing
	// invoice ID which is essentially used as a primary key. With each
	// invoice inserted, the primary key is incremented by one. This key is
	// stored within the invoiceIndexBucket. Within the invoiceBucket
	// invoices are uniquely identified by the invoice ID.
	numInvoicesKey = []byte("nik")

	// zeroPreimage is an empty preimage that we use when we have an external
	// preimage for the invoice. We initialize it here to allow for easy
	// comparisons.
	zeroPreimage [32]byte

	// zeroHash is an empty hash for easy comparisons.
	zeroHash [sha256.Size]byte

	// addIndexBucket is an index bucket that we'll use to create a
	// monotonically increasing set of add indexes. Each time we add a new
	// invoice, this sequence number will be incremented and then populated
	// within the new invoice.
	//
	// In addition to this sequence number, we map:
	//
	//   addIndexNo => invoiceKey
	addIndexBucket = []byte("invoice-add-index")

	// settleIndexBucket is an index bucket that we'll use to create a
	// monotonically increasing integer for tracking a "settle index". Each
	// time an invoice is settled, this sequence number will be incremented
	// as populate within the newly settled invoice.
	//
	// In addition to this sequence number, we map:
	//
	//   settleIndexNo => invoiceKey
	settleIndexBucket = []byte("invoice-settle-index")
)

const (
	// MaxMemoSize is maximum size of the memo field within invoices stored
	// in the database.
	MaxMemoSize = 1024

	// MaxReceiptSize is the maximum size of the payment receipt stored
	// within the database along side incoming/outgoing invoices.
	MaxReceiptSize = 1024

	// MaxPaymentRequestSize is the max size of a payment request for
	// this invoice.
	// TODO(halseth): determine the max length payment request when field
	// lengths are final.
	MaxPaymentRequestSize = 4096
)

// ContractTerm is a companion struct to the Invoice struct. This struct houses
// the necessary conditions required before the invoice can be considered fully
// settled by the payee.
type ContractTerm struct {
	// ExternalPreimage indicates if the preimage for this hash is
	// stored externally and must be retrieved.
	ExternalPreimage bool

	// PaymentHash is the hash that locks the HTLC for this payment.
	PaymentHash [sha256.Size]byte

	// PaymentPreimage is the preimage which is to be revealed in the
	// occasion that an HTLC paying to the hash of this preimage is
	// extended.
	PaymentPreimage [32]byte

	// Value is the expected amount of milli-satoshis to be paid to an HTLC
	// which can be satisfied by the above preimage.
	Value lnwire.MilliSatoshi

	// Settled indicates if this particular contract term has been fully
	// settled by the payer.
	Settled bool
}

// Invoice is a payment invoice generated by a payee in order to request
// payment for some good or service. The inclusion of invoices within Lightning
// creates a payment work flow for merchants very similar to that of the
// existing financial system within PayPal, etc.  Invoices are added to the
// database when a payment is requested, then can be settled manually once the
// payment is received at the upper layer. For record keeping purposes,
// invoices are never deleted from the database, instead a bit is toggled
// denoting the invoice has been fully settled. Within the database, all
// invoices must have a unique payment hash which is generated by taking the
// sha256 of the payment preimage.
type Invoice struct {
	// Memo is an optional memo to be stored along side an invoice.  The
	// memo may contain further details pertaining to the invoice itself,
	// or any other message which fits within the size constraints.
	Memo []byte

	// Receipt is an optional field dedicated for storing a
	// cryptographically binding receipt of payment.
	//
	// TODO(roasbeef): document scheme.
	Receipt []byte

	// PaymentRequest is an optional field where a payment request created
	// for this invoice can be stored.
	PaymentRequest []byte

	// CreationDate is the exact time the invoice was created.
	CreationDate time.Time

	// SettleDate is the exact time the invoice was settled.
	SettleDate time.Time

	// Terms are the contractual payment terms of the invoice. Once all the
	// terms have been satisfied by the payer, then the invoice can be
	// considered fully fulfilled.
	//
	// TODO(roasbeef): later allow for multiple terms to fulfill the final
	// invoice: payment fragmentation, etc.
	Terms ContractTerm

	// AddIndex is an auto-incrementing integer that acts as a
	// monotonically increasing sequence number for all invoices created.
	// Clients can then use this field as a "checkpoint" of sorts when
	// implementing a streaming RPC to notify consumers of instances where
	// an invoice has been added before they re-connected.
	//
	// NOTE: This index starts at 1.
	AddIndex uint64

	// SettleIndex is an auto-incrementing integer that acts as a
	// monotonically increasing sequence number for all settled invoices.
	// Clients can then use this field as a "checkpoint" of sorts when
	// implementing a streaming RPC to notify consumers of instances where
	// an invoice has been settled before they re-connected.
	//
	// NOTE: This index starts at 1.
	SettleIndex uint64

	// AmtPaid is the final amount that we ultimately accepted for pay for
	// this invoice. We specify this value independently as it's possible
	// that the invoice originally didn't specify an amount, or the sender
	// overpaid.
	AmtPaid lnwire.MilliSatoshi
}

// getPaymentHash retrieves the payment hash for a given invoice,
// either by calculating it from the preimage, or using the given
// hash for invoices with external preimages.
func getPaymentHash(i *Invoice) ([32]byte, error) {
	var paymentHash [32]byte

	if i.Terms.ExternalPreimage {
		if bytes.Equal(i.Terms.PaymentHash[:], zeroHash[:]) {
			return zeroHash, fmt.Errorf("Invoices with ExternalPreimage must " +
				"have a locally defined PaymentHash.")
		}

		// For external preimages, we rely on a provided hash
		paymentHash = i.Terms.PaymentHash
	} else {
		if bytes.Equal(i.Terms.PaymentPreimage[:], zeroPreimage[:]) {
			return zeroHash, fmt.Errorf("Invoices must have a preimage or" +
				"use ExternalPreimages")
		}

		// For local preimages, we calculate the hash ourselves
		paymentHash = sha256.Sum256(i.Terms.PaymentPreimage[:])
	}

	return paymentHash, nil
}

func validateInvoice(i *Invoice) error {
	if len(i.Memo) > MaxMemoSize {
		return fmt.Errorf("max length a memo is %v, and invoice "+
			"of length %v was provided", MaxMemoSize, len(i.Memo))
	}
	if len(i.Receipt) > MaxReceiptSize {
		return fmt.Errorf("max length a receipt is %v, and invoice "+
			"of length %v was provided", MaxReceiptSize,
			len(i.Receipt))
	}
	if len(i.PaymentRequest) > MaxPaymentRequestSize {
		return fmt.Errorf("max length of payment request is %v, length "+
			"provided was %v", MaxPaymentRequestSize,
			len(i.PaymentRequest))
	}
	return nil
}

// AddInvoice inserts the targeted invoice into the database. If the invoice
// has *any* payment hashes which already exists within the database, then the
// insertion will be aborted and rejected due to the strict policy banning any
// duplicate payment hashes.
func (d *DB) AddInvoice(newInvoice *Invoice) (uint64, error) {
	if err := validateInvoice(newInvoice); err != nil {
		return 0, err
	}

	var invoiceAddIndex uint64
	err := d.Update(func(tx *bolt.Tx) error {
		invoices, err := tx.CreateBucketIfNotExists(invoiceBucket)
		if err != nil {
			return err
		}

		invoiceIndex, err := invoices.CreateBucketIfNotExists(
			invoiceIndexBucket,
		)
		if err != nil {
			return err
		}
		addIndex, err := invoices.CreateBucketIfNotExists(
			addIndexBucket,
		)
		if err != nil {
			return err
		}

		paymentHash, err := getPaymentHash(newInvoice)
		if err != nil {
			return err
		}

		// Ensure that an invoice an identical payment hash doesn't
		// already exist within the index.
		if invoiceIndex.Get(paymentHash[:]) != nil {
			return ErrDuplicateInvoice
		}

		// If the current running payment ID counter hasn't yet been
		// created, then create it now.
		var invoiceNum uint32
		invoiceCounter := invoiceIndex.Get(numInvoicesKey)
		if invoiceCounter == nil {
			var scratch [4]byte
			byteOrder.PutUint32(scratch[:], invoiceNum)
			err := invoiceIndex.Put(numInvoicesKey, scratch[:])
			if err != nil {
				return nil
			}
		} else {
			invoiceNum = byteOrder.Uint32(invoiceCounter)
		}

		newIndex, err := putInvoice(
			invoices, invoiceIndex, addIndex, newInvoice, invoiceNum,
		)
		if err != nil {
			return err
		}

		invoiceAddIndex = newIndex
		return nil
	})
	if err != nil {
		return 0, err
	}

	return invoiceAddIndex, err
}

// InvoicesAddedSince can be used by callers to seek into the event time series
// of all the invoices added in the database. The specified sinceAddIndex
// should be the highest add index that the caller knows of. This method will
// return all invoices with an add index greater than the specified
// sinceAddIndex.
//
// NOTE: The index starts from 1, as a result. We enforce that specifying a
// value below the starting index value is a noop.
func (d *DB) InvoicesAddedSince(sinceAddIndex uint64) ([]Invoice, error) {
	var newInvoices []Invoice

	// If an index of zero was specified, then in order to maintain
	// backwards compat, we won't send out any new invoices.
	if sinceAddIndex == 0 {
		return newInvoices, nil
	}

	var startIndex [8]byte
	byteOrder.PutUint64(startIndex[:], sinceAddIndex)

	err := d.DB.View(func(tx *bolt.Tx) error {
		invoices := tx.Bucket(invoiceBucket)
		if invoices == nil {
			return ErrNoInvoicesCreated
		}

		addIndex := invoices.Bucket(addIndexBucket)
		if addIndex == nil {
			return ErrNoInvoicesCreated
		}

		// We'll now run through each entry in the add index starting
		// at our starting index. We'll continue until we reach the
		// very end of the current key space.
		invoiceCursor := addIndex.Cursor()

		// We'll seek to the starting index, then manually advance the
		// cursor in order to skip the entry with the since add index.
		invoiceCursor.Seek(startIndex[:])
		addSeqNo, invoiceKey := invoiceCursor.Next()

		for ; addSeqNo != nil && bytes.Compare(addSeqNo, startIndex[:]) > 0; addSeqNo, invoiceKey = invoiceCursor.Next() {

			// For each key found, we'll look up the actual
			// invoice, then accumulate it into our return value.
			invoice, err := fetchInvoice(invoiceKey, invoices)
			if err != nil {
				return err
			}

			newInvoices = append(newInvoices, invoice)
		}

		return nil
	})
	switch {
	// If no invoices have been created, then we'll return the empty set of
	// invoices.
	case err == ErrNoInvoicesCreated:

	case err != nil:
		return nil, err
	}

	return newInvoices, nil
}

// LookupInvoice attempts to look up an invoice according to its 32 byte
// payment hash. If an invoice which can settle the HTLC identified by the
// passed payment hash isn't found, then an error is returned. Otherwise, the
// full invoice is returned. Before setting the incoming HTLC, the values
// SHOULD be checked to ensure the payer meets the agreed upon contractual
// terms of the payment.
func (d *DB) LookupInvoice(paymentHash [32]byte) (Invoice, error) {
	var invoice Invoice
	err := d.View(func(tx *bolt.Tx) error {
		invoices := tx.Bucket(invoiceBucket)
		if invoices == nil {
			return ErrNoInvoicesCreated
		}
		invoiceIndex := invoices.Bucket(invoiceIndexBucket)
		if invoiceIndex == nil {
			return ErrNoInvoicesCreated
		}

		// Check the invoice index to see if an invoice paying to this
		// hash exists within the DB.
		invoiceNum := invoiceIndex.Get(paymentHash[:])
		if invoiceNum == nil {
			return ErrInvoiceNotFound
		}

		// An invoice matching the payment hash has been found, so
		// retrieve the record of the invoice itself.
		i, err := fetchInvoice(invoiceNum, invoices)
		if err != nil {
			return err
		}
		invoice = i

		return nil
	})
	if err != nil {
		return invoice, err
	}

	return invoice, nil
}

// FetchAllInvoices returns all invoices currently stored within the database.
// If the pendingOnly param is true, then only unsettled invoices will be
// returned, skipping all invoices that are fully settled.
func (d *DB) FetchAllInvoices(pendingOnly bool) ([]Invoice, error) {
	var invoices []Invoice

	err := d.View(func(tx *bolt.Tx) error {
		invoiceB := tx.Bucket(invoiceBucket)
		if invoiceB == nil {
			return ErrNoInvoicesCreated
		}

		// Iterate through the entire key space of the top-level
		// invoice bucket. If key with a non-nil value stores the next
		// invoice ID which maps to the corresponding invoice.
		return invoiceB.ForEach(func(k, v []byte) error {
			if v == nil {
				return nil
			}

			invoiceReader := bytes.NewReader(v)
			invoice, err := deserializeInvoice(invoiceReader)
			if err != nil {
				return err
			}

			if pendingOnly && invoice.Terms.Settled {
				return nil
			}

			invoices = append(invoices, invoice)

			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return invoices, nil
}

// InvoiceQuery represents a query to the invoice database. The query allows a
// caller to retrieve all invoices starting from a particular add index and
// limit the number of results returned.
type InvoiceQuery struct {
	// IndexOffset is the offset within the add indices to start at. This
	// can be used to start the response at a particular invoice.
	IndexOffset uint64

	// NumMaxInvoices is the maximum number of invoices that should be
	// starting from the add index.
	NumMaxInvoices uint64

	// PendingOnly, if set, returns unsettled invoices starting from the
	// add index.
	PendingOnly bool

	// Reversed, if set, indicates that the invoices returned should start
	// from the IndexOffset and go backwards.
	Reversed bool
}

// InvoiceSlice is the response to a invoice query. It includes the original
// query, the set of invoices that match the query, and an integer which
// represents the offset index of the last item in the set of returned invoices.
// This integer allows callers to resume their query using this offset in the
// event that the query's response exceeds the maximum number of returnable
// invoices.
type InvoiceSlice struct {
	InvoiceQuery

	// Invoices is the set of invoices that matched the query above.
	Invoices []Invoice

	// FirstIndexOffset is the index of the first element in the set of
	// returned Invoices above. Callers can use this to resume their query
	// in the event that the slice has too many events to fit into a single
	// response.
	FirstIndexOffset uint64

	// LastIndexOffset is the index of the last element in the set of
	// returned Invoices above. Callers can use this to resume their query
	// in the event that the slice has too many events to fit into a single
	// response.
	LastIndexOffset uint64
}

// QueryInvoices allows a caller to query the invoice database for invoices
// within the specified add index range.
func (d *DB) QueryInvoices(q InvoiceQuery) (InvoiceSlice, error) {
	resp := InvoiceSlice{
		InvoiceQuery: q,
	}

	err := d.View(func(tx *bolt.Tx) error {
		// If the bucket wasn't found, then there aren't any invoices
		// within the database yet, so we can simply exit.
		invoices := tx.Bucket(invoiceBucket)
		if invoices == nil {
			return ErrNoInvoicesCreated
		}
		invoiceAddIndex := invoices.Bucket(addIndexBucket)
		if invoiceAddIndex == nil {
			return ErrNoInvoicesCreated
		}

		// keyForIndex is a helper closure that retrieves the invoice
		// key for the given add index of an invoice.
		keyForIndex := func(c *bolt.Cursor, index uint64) []byte {
			var keyIndex [8]byte
			byteOrder.PutUint64(keyIndex[:], index)
			_, invoiceKey := c.Seek(keyIndex[:])
			return invoiceKey
		}

		// nextKey is a helper closure to determine what the next
		// invoice key is when iterating over the invoice add index.
		nextKey := func(c *bolt.Cursor) ([]byte, []byte) {
			if q.Reversed {
				return c.Prev()
			}
			return c.Next()
		}

		// We'll be using a cursor to seek into the database and return
		// a slice of invoices. We'll need to determine where to start
		// our cursor depending on the parameters set within the query.
		c := invoiceAddIndex.Cursor()
		invoiceKey := keyForIndex(c, q.IndexOffset+1)

		// If the query is specifying reverse iteration, then we must
		// handle a few offset cases.
		if q.Reversed {
			switch q.IndexOffset {

			// This indicates the default case, where no offset was
			// specified. In that case we just start from the last
			// invoice.
			case 0:
				_, invoiceKey = c.Last()

			// This indicates the offset being set to the very
			// first invoice. Since there are no invoices before
			// this offset, and the direction is reversed, we can
			// return without adding any invoices to the response.
			case 1:
				return nil

			// Otherwise we start iteration at the invoice prior to
			// the offset.
			default:
				invoiceKey = keyForIndex(c, q.IndexOffset-1)
			}
		}

		// If we know that a set of invoices exists, then we'll begin
		// our seek through the bucket in order to satisfy the query.
		// We'll continue until either we reach the end of the range, or
		// reach our max number of invoices.
		for ; invoiceKey != nil; _, invoiceKey = nextKey(c) {
			// If our current return payload exceeds the max number
			// of invoices, then we'll exit now.
			if uint64(len(resp.Invoices)) >= q.NumMaxInvoices {
				break
			}

			invoice, err := fetchInvoice(invoiceKey, invoices)
			if err != nil {
				return err
			}

			// Skip any settled invoices if the caller is only
			// interested in unsettled.
			if q.PendingOnly && invoice.Terms.Settled {
				continue
			}

			// At this point, we've exhausted the offset, so we'll
			// begin collecting invoices found within the range.
			resp.Invoices = append(resp.Invoices, invoice)
		}

		// If we iterated through the add index in reverse order, then
		// we'll need to reverse the slice of invoices to return them in
		// forward order.
		if q.Reversed {
			numInvoices := len(resp.Invoices)
			for i := 0; i < numInvoices/2; i++ {
				opposite := numInvoices - i - 1
				resp.Invoices[i], resp.Invoices[opposite] =
					resp.Invoices[opposite], resp.Invoices[i]
			}
		}

		return nil
	})
	if err != nil && err != ErrNoInvoicesCreated {
		return resp, err
	}

	// Finally, record the indexes of the first and last invoices returned
	// so that the caller can resume from this point later on.
	if len(resp.Invoices) > 0 {
		resp.FirstIndexOffset = resp.Invoices[0].AddIndex
		resp.LastIndexOffset = resp.Invoices[len(resp.Invoices)-1].AddIndex
	}

	return resp, nil
}

// SettleInvoice attempts to mark an invoice corresponding to the passed
// payment hash as fully settled. If an invoice matching the passed payment
// hash doesn't existing within the database, then the action will fail with a
// "not found" error.
func (d *DB) SettleInvoice(paymentHash [32]byte,
	amtPaid lnwire.MilliSatoshi) (*Invoice, error) {

	var settledInvoice *Invoice
	err := d.Update(func(tx *bolt.Tx) error {
		invoices, err := tx.CreateBucketIfNotExists(invoiceBucket)
		if err != nil {
			return err
		}
		invoiceIndex, err := invoices.CreateBucketIfNotExists(
			invoiceIndexBucket,
		)
		if err != nil {
			return err
		}
		settleIndex, err := invoices.CreateBucketIfNotExists(
			settleIndexBucket,
		)
		if err != nil {
			return err
		}

		// Check the invoice index to see if an invoice paying to this
		// hash exists within the DB.
		invoiceNum := invoiceIndex.Get(paymentHash[:])
		if invoiceNum == nil {
			return ErrInvoiceNotFound
		}
		invoice, err := settleInvoice(
			invoices, settleIndex, invoiceNum, amtPaid,
		)
		if err != nil {
			return err
		}

		settledInvoice = invoice
		return nil
	})
	if err != nil {
		return nil, err
	}

	return settledInvoice, nil
}

// addInvoicePreimage attempts to update an invoice with an External Preimage
// to save that preimage locally, allowing us to re-use it for duplicate
// payments and provide it to listeners via LookupInvoice. If an invoice
// matching this hash does not exist, it will fail with a "not found" error.
func (d *DB) AddInvoicePreimage(paymentHash [32]byte,
	paymentPreimage [32]byte) error {
	return d.Update(func(tx *bolt.Tx) error {
		invoices, err := tx.CreateBucketIfNotExists(invoiceBucket)
		if err != nil {
			return err
		}
		invoiceIndex, err := invoices.CreateBucketIfNotExists(
			invoiceIndexBucket,
		)
		if err != nil {
			return err
		}

		// Check the invoice index to see if an invoice paying to this
		// hash exists within the DB.
		invoiceNum := invoiceIndex.Get(paymentHash[:])
		if invoiceNum == nil {
			return ErrInvoiceNotFound
		}

		return addInvoicePreimage(invoices, invoiceNum, paymentPreimage)
	})
}

// InvoicesSettledSince can be used by callers to catch up any settled invoices
// they missed within the settled invoice time series. We'll return all known
// settled invoice that have a settle index higher than the passed
// sinceSettleIndex.
//
// NOTE: The index starts from 1, as a result. We enforce that specifying a
// value below the starting index value is a noop.
func (d *DB) InvoicesSettledSince(sinceSettleIndex uint64) ([]Invoice, error) {
	var settledInvoices []Invoice

	// If an index of zero was specified, then in order to maintain
	// backwards compat, we won't send out any new invoices.
	if sinceSettleIndex == 0 {
		return settledInvoices, nil
	}

	var startIndex [8]byte
	byteOrder.PutUint64(startIndex[:], sinceSettleIndex)

	err := d.DB.View(func(tx *bolt.Tx) error {
		invoices := tx.Bucket(invoiceBucket)
		if invoices == nil {
			return ErrNoInvoicesCreated
		}

		settleIndex := invoices.Bucket(settleIndexBucket)
		if settleIndex == nil {
			return ErrNoInvoicesCreated
		}

		// We'll now run through each entry in the add index starting
		// at our starting index. We'll continue until we reach the
		// very end of the current key space.
		invoiceCursor := settleIndex.Cursor()

		// We'll seek to the starting index, then manually advance the
		// cursor in order to skip the entry with the since add index.
		invoiceCursor.Seek(startIndex[:])
		seqNo, invoiceKey := invoiceCursor.Next()

		for ; seqNo != nil && bytes.Compare(seqNo, startIndex[:]) > 0; seqNo, invoiceKey = invoiceCursor.Next() {

			// For each key found, we'll look up the actual
			// invoice, then accumulate it into our return value.
			invoice, err := fetchInvoice(invoiceKey, invoices)
			if err != nil {
				return err
			}

			settledInvoices = append(settledInvoices, invoice)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return settledInvoices, nil
}

func putInvoice(invoices, invoiceIndex, addIndex *bolt.Bucket,
	i *Invoice, invoiceNum uint32) (uint64, error) {

	// Create the invoice key which is just the big-endian representation
	// of the invoice number.
	var invoiceKey [4]byte
	byteOrder.PutUint32(invoiceKey[:], invoiceNum)

	// Increment the num invoice counter index so the next invoice bares
	// the proper ID.
	var scratch [4]byte
	invoiceCounter := invoiceNum + 1
	byteOrder.PutUint32(scratch[:], invoiceCounter)
	if err := invoiceIndex.Put(numInvoicesKey, scratch[:]); err != nil {
		return 0, err
	}

	paymentHash, err := getPaymentHash(i)
	if err != nil {
		return 0, err
	}

	// Add the payment hash to the invoice index. This will let us quickly
	// identify if we can settle an incoming payment, and also to possibly
	// allow a single invoice to have multiple payment installations.
	err = invoiceIndex.Put(paymentHash[:], invoiceKey[:])
	if err != nil {
		return 0, err
	}

	// Next, we'll obtain the next add invoice index (sequence
	// number), so we can properly place this invoice within this
	// event stream.
	nextAddSeqNo, err := addIndex.NextSequence()
	if err != nil {
		return 0, err
	}

	// With the next sequence obtained, we'll updating the event series in
	// the add index bucket to map this current add counter to the index of
	// this new invoice.
	var seqNoBytes [8]byte
	byteOrder.PutUint64(seqNoBytes[:], nextAddSeqNo)
	if err := addIndex.Put(seqNoBytes[:], invoiceKey[:]); err != nil {
		return 0, err
	}

	i.AddIndex = nextAddSeqNo

	// Finally, serialize the invoice itself to be written to the disk.
	var buf bytes.Buffer
	if err := serializeInvoice(&buf, i); err != nil {
		return 0, nil
	}

	if err := invoices.Put(invoiceKey[:], buf.Bytes()); err != nil {
		return 0, err
	}

	return nextAddSeqNo, nil
}

func serializeInvoice(w io.Writer, i *Invoice) error {
	if err := wire.WriteVarBytes(w, 0, i.Memo[:]); err != nil {
		return err
	}
	if err := wire.WriteVarBytes(w, 0, i.Receipt[:]); err != nil {
		return err
	}
	if err := wire.WriteVarBytes(w, 0, i.PaymentRequest[:]); err != nil {
		return err
	}

	birthBytes, err := i.CreationDate.MarshalBinary()
	if err != nil {
		return err
	}

	if err := wire.WriteVarBytes(w, 0, birthBytes); err != nil {
		return err
	}

	settleBytes, err := i.SettleDate.MarshalBinary()
	if err != nil {
		return err
	}

	if err := wire.WriteVarBytes(w, 0, settleBytes); err != nil {
		return err
	}

	// if the preimage is defined locally, it will be persisted here.
	// Otherwise, the 32 bytes of 0 will be persisted.
	if _, err := w.Write(i.Terms.PaymentPreimage[:]); err != nil {
		return err
	}

	var paymentHash [sha256.Size]byte
	if i.Terms.ExternalPreimage {
		paymentHash, err = getPaymentHash(i)
		if err != nil {
			return err
		}
	} else {
		paymentHash = zeroHash
	}

	if _, err := w.Write(paymentHash[:]); err != nil {
		return err
	}

	var scratch [8]byte
	byteOrder.PutUint64(scratch[:], uint64(i.Terms.Value))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	if err := binary.Write(w, byteOrder, i.Terms.Settled); err != nil {
		return err
	}

	if err := binary.Write(w, byteOrder, i.AddIndex); err != nil {
		return err
	}
	if err := binary.Write(w, byteOrder, i.SettleIndex); err != nil {
		return err
	}
	if err := binary.Write(w, byteOrder, int64(i.AmtPaid)); err != nil {
		return err
	}
	if err := binary.Write(w, byteOrder, i.Terms.ExternalPreimage); err != nil {
		return err
	}

	return nil
}

func fetchInvoice(invoiceNum []byte, invoices *bolt.Bucket) (Invoice, error) {
	invoiceBytes := invoices.Get(invoiceNum)
	if invoiceBytes == nil {
		return Invoice{}, ErrInvoiceNotFound
	}

	invoiceReader := bytes.NewReader(invoiceBytes)

	return deserializeInvoice(invoiceReader)
}

func deserializeInvoice(r io.Reader) (Invoice, error) {
	var err error
	invoice := Invoice{}

	// TODO(roasbeef): use read full everywhere
	invoice.Memo, err = wire.ReadVarBytes(r, 0, MaxMemoSize, "")
	if err != nil {
		return invoice, err
	}
	invoice.Receipt, err = wire.ReadVarBytes(r, 0, MaxReceiptSize, "")
	if err != nil {
		return invoice, err
	}

	invoice.PaymentRequest, err = wire.ReadVarBytes(r, 0, MaxPaymentRequestSize, "")
	if err != nil {
		return invoice, err
	}

	birthBytes, err := wire.ReadVarBytes(r, 0, 300, "birth")
	if err != nil {
		return invoice, err
	}
	if err := invoice.CreationDate.UnmarshalBinary(birthBytes); err != nil {
		return invoice, err
	}

	settledBytes, err := wire.ReadVarBytes(r, 0, 300, "settled")
	if err != nil {
		return invoice, err
	}
	if err := invoice.SettleDate.UnmarshalBinary(settledBytes); err != nil {
		return invoice, err
	}

	if _, err := io.ReadFull(r, invoice.Terms.PaymentPreimage[:]); err != nil {
		return invoice, err
	}

	if _, err := io.ReadFull(r, invoice.Terms.PaymentHash[:]); err != nil {
		return invoice, err
	}

	var scratch [8]byte
	if _, err := io.ReadFull(r, scratch[:]); err != nil {
		return invoice, err
	}
	invoice.Terms.Value = lnwire.MilliSatoshi(byteOrder.Uint64(scratch[:]))

	if err := binary.Read(r, byteOrder, &invoice.Terms.Settled); err != nil {
		return invoice, err
	}

	if err := binary.Read(r, byteOrder, &invoice.AddIndex); err != nil {
		return invoice, err
	}
	if err := binary.Read(r, byteOrder, &invoice.SettleIndex); err != nil {
		return invoice, err
	}
	if err := binary.Read(r, byteOrder, &invoice.AmtPaid); err != nil {
		return invoice, err
	}

	if err := binary.Read(r, byteOrder, &invoice.Terms.ExternalPreimage); err != nil {
		return invoice, err
	}

	if !invoice.Terms.ExternalPreimage {
		// If the Preimage is internal, we do not want a local copy of the hash
		invoice.Terms.PaymentHash = zeroHash
	}

	return invoice, nil
}

func settleInvoice(invoices, settleIndex *bolt.Bucket, invoiceNum []byte,
	amtPaid lnwire.MilliSatoshi) (*Invoice, error) {

	invoice, err := fetchInvoice(invoiceNum, invoices)
	if err != nil {
		return nil, err
	}

	// Add idempotency to duplicate settles, return here to avoid
	// overwriting the previous info.
	if invoice.Terms.Settled {
		return &invoice, nil
	}

	// Now that we know the invoice hasn't already been settled, we'll
	// update the settle index so we can place this settle event in the
	// proper location within our time series.
	nextSettleSeqNo, err := settleIndex.NextSequence()
	if err != nil {
		return nil, err
	}

	var seqNoBytes [8]byte
	byteOrder.PutUint64(seqNoBytes[:], nextSettleSeqNo)
	if err := settleIndex.Put(seqNoBytes[:], invoiceNum); err != nil {
		return nil, err
	}

	invoice.AmtPaid = amtPaid
	invoice.Terms.Settled = true
	invoice.SettleDate = time.Now()
	invoice.SettleIndex = nextSettleSeqNo

	var buf bytes.Buffer
	if err := serializeInvoice(&buf, &invoice); err != nil {
		return nil, err
	}

	if err := invoices.Put(invoiceNum[:], buf.Bytes()); err != nil {
		return nil, err
	}

	return &invoice, nil
}

// addInvoicePreimage adds a preimage to an existing invoice that has an
// External Preimage. It will only update invoices with External Preimage
// marked to avoid overwriting otherwise good invoice preimages.
func addInvoicePreimage(invoices *bolt.Bucket, invoiceNum []byte,
	paymentPreimage [32]byte) error {
	invoice, err := fetchInvoice(invoiceNum, invoices)
	if err != nil {
		return err
	}

	// Only allow setting a preimage on invoices that are true External Preimage
	// invoices
	if !invoice.Terms.ExternalPreimage {
		return fmt.Errorf("Invoices without an ExternalPreimage flag cannot have " +
			"their preimage modified.")
	}

	var zeroPreimage [32]byte

	// If we already have a preimage we do not want to overwrite it.
	if !bytes.Equal(zeroPreimage[:], invoice.Terms.PaymentPreimage[:]) {
		return fmt.Errorf("Attempted to overwrite preimage on ExternalPreimage "+
			"invoice with: %v", paymentPreimage)
	}

	invoice.Terms.PaymentPreimage = paymentPreimage

	var buf bytes.Buffer
	if err := serializeInvoice(&buf, &invoice); err != nil {
		return nil
	}

	return invoices.Put(invoiceNum[:], buf.Bytes())
}
