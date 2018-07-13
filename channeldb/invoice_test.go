package channeldb

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"reflect"
	"testing"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lnwire"
)

func randInvoice(value lnwire.MilliSatoshi) (*Invoice, error) {
	var pre [32]byte
	if _, err := rand.Read(pre[:]); err != nil {
		return nil, err
	}

	i := &Invoice{
		// Use single second precision to avoid false positive test
		// failures due to the monotonic time component.
		CreationDate: time.Unix(time.Now().Unix(), 0),
		Terms: ContractTerm{
			PaymentPreimage: pre,
			Value:           value,
		},
	}
	i.Memo = []byte("memo")
	i.Receipt = []byte("receipt")

	// Create a random byte slice of MaxPaymentRequestSize bytes to be used
	// as a dummy paymentrequest, and  determine if it should be set based
	// on one of the random bytes.
	var r [MaxPaymentRequestSize]byte
	if _, err := rand.Read(r[:]); err != nil {
		return nil, err
	}
	if r[0]&1 == 0 {
		i.PaymentRequest = r[:]
	} else {
		i.PaymentRequest = []byte("")
	}

	return i, nil
}

func TestInvoiceWorkflow(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test db: %v", err)
	}

	// Create a fake invoice which we'll use several times in the tests
	// below.
	fakeInvoice := &Invoice{
		// Use single second precision to avoid false positive test
		// failures due to the monotonic time component.
		CreationDate: time.Unix(time.Now().Unix(), 0),
	}
	fakeInvoice.Memo = []byte("memo")
	fakeInvoice.Receipt = []byte("receipt")
	fakeInvoice.PaymentRequest = []byte("")
	copy(fakeInvoice.Terms.PaymentPreimage[:], rev[:])
	fakeInvoice.Terms.Value = lnwire.NewMSatFromSatoshis(10000)

	// Add the invoice to the database, this should succeed as there aren't
	// any existing invoices within the database with the same payment
	// hash.
	if err := db.AddInvoice(fakeInvoice); err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Attempt to retrieve the invoice which was just added to the
	// database. It should be found, and the invoice returned should be
	// identical to the one created above.
	paymentHash := sha256.Sum256(fakeInvoice.Terms.PaymentPreimage[:])
	dbInvoice, err := db.LookupInvoice(paymentHash)
	if err != nil {
		t.Fatalf("unable to find invoice: %v", err)
	}
	if !reflect.DeepEqual(fakeInvoice, dbInvoice) {
		t.Fatalf("invoice fetched from db doesn't match original %v vs %v",
			spew.Sdump(fakeInvoice), spew.Sdump(dbInvoice))
	}

	// Settle the invoice, the version retrieved from the database should
	// now have the settled bit toggle to true and a non-default
	// SettledDate
	if err := db.SettleInvoice(paymentHash); err != nil {
		t.Fatalf("unable to settle invoice: %v", err)
	}
	dbInvoice2, err := db.LookupInvoice(paymentHash)
	if err != nil {
		t.Fatalf("unable to fetch invoice: %v", err)
	}
	if !dbInvoice2.Terms.Settled {
		t.Fatalf("invoice should now be settled but isn't")
	}

	if dbInvoice2.SettleDate.IsZero() {
		t.Fatalf("invoice should have non-zero SettledDate but isn't")
	}

	// Attempt to insert generated above again, this should fail as
	// duplicates are rejected by the processing logic.
	if err := db.AddInvoice(fakeInvoice); err != ErrDuplicateInvoice {
		t.Fatalf("invoice insertion should fail due to duplication, "+
			"instead %v", err)
	}

	// Attempt to look up a non-existent invoice, this should also fail but
	// with a "not found" error.
	var fakeHash [32]byte
	if _, err := db.LookupInvoice(fakeHash); err != ErrInvoiceNotFound {
		t.Fatalf("lookup should have failed, instead %v", err)
	}

	// Add an invoice with an external preimage
	// Create a fake invoice with an external preimage which we'll use several times in the tests
	// below.
	fakeExternalPreimageInvoice := &Invoice{
		// Use single second precision to avoid false positive test
		// failures due to the monotonic time component.
		CreationDate: time.Unix(time.Now().Unix(), 0),
	}
	fakeExternalPreimageInvoice.Memo = []byte("memo")
	fakeExternalPreimageInvoice.Receipt = []byte("receipt")
	fakeExternalPreimageInvoice.PaymentRequest = []byte("")
	fakeExternalPreimageInvoice.Terms.ExternalPreimage = true
	externalPreimagePaymentHash := sha256.Sum256([]byte("fake preimage"))
	fakeExternalPreimageInvoice.Terms.PaymentHash = externalPreimagePaymentHash
	fakeExternalPreimageInvoice.Terms.Value = lnwire.NewMSatFromSatoshis(10000)

	// Add the invoice to the database, this should succeed as there aren't
	// any existing invoices within the database with the same payment
	// hash.
	if err := db.AddInvoice(fakeExternalPreimageInvoice); err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Attempt to retrieve the invoice which was just added to the
	// database. It should be found, and the invoice returned should be
	// identical to the one created above.
	dbExternalPreimageInvoice, err := db.LookupInvoice(externalPreimagePaymentHash)
	if err != nil {
		t.Fatalf("unable to find invoice: %v", err)
	}
	if !reflect.DeepEqual(fakeExternalPreimageInvoice, dbExternalPreimageInvoice) {
		t.Fatalf("invoice fetched from db doesn't match original %v vs %v",
			spew.Sdump(fakeExternalPreimageInvoice), spew.Sdump(dbExternalPreimageInvoice))
	}

	// Attempt to insert generated above again, this should fail as
	// duplicates are rejected by the processing logic.
	if err := db.AddInvoice(fakeExternalPreimageInvoice); err != ErrDuplicateInvoice {
		t.Fatalf("invoice insertion should fail due to duplication, "+
			"instead %v", err)
	}

	// Attempt to add a preimage to the existing invoice, this should succeed
	var fakePreimage [32]byte
	copy(fakePreimage[:], []byte("fake preimage"))
	if err := db.AddInvoicePreimage(externalPreimagePaymentHash, fakePreimage); err != nil {
		t.Fatalf("unable to add preimage: %v", err)
	}
	dbLocalPreimageInvoice, err := db.LookupInvoice(externalPreimagePaymentHash)
	if err != nil {
		t.Fatalf("unable to find invoice: %v", err)
	}
	if !bytes.Equal(fakePreimage[:], dbLocalPreimageInvoice.Terms.PaymentPreimage[:]) {
		t.Fatalf("invoice fetched from db doesn't have local preimage: %v vs %v",
			fakePreimage, dbLocalPreimageInvoice.Terms.PaymentPreimage)
	}

	// Attempt to add a preimage to the same invoice again, this should fail
	var anotherFakePreimage [32]byte
	copy(anotherFakePreimage[:], []byte("another fake"))
	if err := db.AddInvoicePreimage(externalPreimagePaymentHash, anotherFakePreimage); err == nil {
		t.Fatalf("invoice overwrote preimage for hash")
	}

	// Attempt to add a preimage to a non-ExternalPreimage invoice, this should fail
	if err := db.AddInvoicePreimage(paymentHash, anotherFakePreimage); err == nil {
		t.Fatalf("invoice wrote preimage for non-ExternalPreimage invoice")
	}

	// Add 100 random invoices.
	const numInvoices = 10
	amt := lnwire.NewMSatFromSatoshis(1000)
	invoices := make([]*Invoice, numInvoices+1)
	for i := 2; i < len(invoices)-1; i++ {
		invoice, err := randInvoice(amt)
		if err != nil {
			t.Fatalf("unable to create invoice: %v", err)
		}

		if err := db.AddInvoice(invoice); err != nil {
			t.Fatalf("unable to add invoice %v", err)
		}

		invoices[i] = invoice
	}

	// Perform a scan to collect all the active invoices.
	dbInvoices, err := db.FetchAllInvoices(false)
	if err != nil {
		t.Fatalf("unable to fetch all invoices: %v", err)
	}

	// The retrieve list of invoices should be identical as since we're
	// using big endian, the invoices should be retrieved in ascending
	// order (and the primary key should be incremented with each
	// insertion).
	for i := 2; i < len(invoices)-1; i++ {
		if !reflect.DeepEqual(invoices[i], dbInvoices[i]) {
			t.Fatalf("retrieved invoices don't match %v vs %v",
				spew.Sdump(invoices[i]),
				spew.Sdump(dbInvoices[i]))
		}
	}
}
