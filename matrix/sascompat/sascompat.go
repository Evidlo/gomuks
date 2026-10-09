// gomuks - A terminal Matrix client written in Go.
// Copyright (C) 2020 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

//go:build cgo

// Package sascompat bolts the to-device request/ready/done handshake onto mautrix v0.11's legacy SAS
// verification, which only knows how to send a bare m.key.verification.start. Modern clients (Element,
// FluffyChat) silently ignore a start that wasn't preceded by request and ready.
//
// It relies on the patched mautrix copy in third_party/mautrix (see GOMUKS_PATCHES.md there) to receive
// encrypted verification events and the OnToDeviceEvent hook.
//
// UGLY HACK: this is a stopgap that pokes around the edges of an abandoned API. mautrix v0.18+ removed the
// legacy implementation in favour of crypto/verificationhelper, which does all of this properly.
// Delete this package when upgrading mautrix.
//
// Known gaps: legacy clients that send a start without a request first are not supported, and verification
// requests are only sent to a single device rather than broadcast to all of them.
package sascompat

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"time"

	sync "github.com/sasha-s/go-deadlock"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"maunium.net/go/gomuks/debug"
)

var (
	typeReady = event.Type{Type: "m.key.verification.ready", Class: event.ToDeviceEventType}
	typeDone  = event.Type{Type: "m.key.verification.done", Class: event.ToDeviceEventType}
)

type readyContent struct {
	FromDevice    id.DeviceID                `json:"from_device"`
	Methods       []event.VerificationMethod `json:"methods"`
	TransactionID string                     `json:"transaction_id"`
}

type transactionContent struct {
	TransactionID string                       `json:"transaction_id"`
	Reason        string                       `json:"reason,omitempty"`
	Code          event.VerificationCancelCode `json:"code,omitempty"`
}

type transaction struct {
	device   *crypto.DeviceIdentity
	hooks    crypto.VerificationHooks
	outgoing bool
	started  bool
}

// HooksFactory creates the verification hooks (i.e. the UI) for a request received from another device.
type HooksFactory func(transactionID string, device *crypto.DeviceIdentity) crypto.VerificationHooks

type Compat struct {
	mach *crypto.OlmMachine
	lock sync.Mutex
	txns map[string]*transaction
}

var (
	instances     = make(map[*crypto.OlmMachine]*Compat)
	instancesLock sync.Mutex
)

// For returns the Compat instance attached to the given machine, creating it if necessary.
func For(mach *crypto.OlmMachine) *Compat {
	instancesLock.Lock()
	defer instancesLock.Unlock()
	compat, ok := instances[mach]
	if !ok {
		compat = &Compat{mach: mach, txns: make(map[string]*transaction)}
		mach.OnToDeviceEvent = compat.handleEvent
		instances[mach] = compat
	}
	return compat
}

// wrappedHooks sends m.key.verification.done on success and forgets the transaction when it ends.
type wrappedHooks struct {
	crypto.VerificationHooks
	compat *Compat
	txnID  string
	device *crypto.DeviceIdentity
}

func (w *wrappedHooks) OnSuccess() {
	w.compat.forget(w.txnID)
	err := w.compat.send(typeDone, w.device, &transactionContent{TransactionID: w.txnID})
	if err != nil {
		debug.Printf("Failed to send verification done for %s: %v", w.txnID, err)
	}
	w.VerificationHooks.OnSuccess()
}

func (w *wrappedHooks) OnCancel(cancelledByUs bool, reason string, code event.VerificationCancelCode) {
	w.compat.forget(w.txnID)
	w.VerificationHooks.OnCancel(cancelledByUs, reason, code)
}

func (c *Compat) wrap(txnID string, device *crypto.DeviceIdentity, hooks crypto.VerificationHooks) *wrappedHooks {
	return &wrappedHooks{VerificationHooks: hooks, compat: c, txnID: txnID, device: device}
}

func (c *Compat) forget(txnID string) {
	c.lock.Lock()
	delete(c.txns, txnID)
	c.lock.Unlock()
}

func (c *Compat) send(evtType event.Type, device *crypto.DeviceIdentity, content interface{}) error {
	_, err := c.mach.Client.SendToDevice(evtType, &mautrix.ReqSendToDevice{
		Messages: map[id.UserID]map[id.DeviceID]*event.Content{
			device.UserID: {device.DeviceID: {Parsed: content}},
		},
	})
	return err
}

func (c *Compat) timeoutAfter(txnID string) {
	time.Sleep(c.mach.DefaultSASTimeout)
	c.lock.Lock()
	txn, ok := c.txns[txnID]
	pending := ok && !txn.started
	c.lock.Unlock()
	if pending {
		_ = c.mach.SendSASVerificationCancel(txn.device.UserID, txn.device.DeviceID, txnID, "Timed out", event.VerificationCancelByTimeout)
		txn.hooks.OnCancel(true, "Timed out waiting for the other device", event.VerificationCancelByTimeout)
	}
}

// Request sends a verification request to the given device. The SAS flow starts once it replies with ready.
func (c *Compat) Request(device *crypto.DeviceIdentity, hooks crypto.VerificationHooks) (string, error) {
	txnID := fmt.Sprintf("gomuks-%d", rand.Int63())
	c.lock.Lock()
	c.txns[txnID] = &transaction{device: device, hooks: c.wrap(txnID, device, hooks), outgoing: true}
	c.lock.Unlock()
	err := c.send(event.ToDeviceVerificationRequest, device, &event.VerificationRequestEventContent{
		FromDevice:    c.mach.Client.DeviceID,
		TransactionID: txnID,
		Methods:       []event.VerificationMethod{event.VerificationMethodSAS},
		Timestamp:     time.Now().UnixMilli(),
	})
	if err != nil {
		c.forget(txnID)
		return "", err
	}
	go c.timeoutAfter(txnID)
	return txnID, nil
}

// Cancel aborts a transaction started by Request or accepted via AcceptFrom.
func (c *Compat) Cancel(txnID string) {
	c.lock.Lock()
	txn, ok := c.txns[txnID]
	delete(c.txns, txnID)
	c.lock.Unlock()
	if !ok {
		return
	}
	reason := "Cancelled by user"
	_ = c.mach.SendSASVerificationCancel(txn.device.UserID, txn.device.DeviceID, txnID, reason, event.VerificationCancelByUser)
	if !txn.started {
		txn.hooks.OnCancel(true, reason, event.VerificationCancelByUser)
		return
	}
	// The machine has no public way to drop a transaction, so feed it a cancel as if it came from the other side.
	c.mach.HandleToDeviceEvent(&event.Event{
		Sender: txn.device.UserID,
		Type:   event.ToDeviceVerificationCancel,
		Content: event.Content{Parsed: &event.VerificationCancelEventContent{
			TransactionID: txnID,
			Reason:        reason,
			Code:          event.VerificationCancelByUser,
		}},
	})
}

// AcceptFrom returns a function for OlmMachine.AcceptVerificationFrom.
//
// The machine calls it both for incoming requests and for incoming starts. The first time a transaction is
// seen it's treated as a request: we reply with ready and wait for the other device to send start, which
// triggers the second call where the transaction is actually accepted. Requests from other users are ignored.
func (c *Compat) AcceptFrom(factory HooksFactory) func(string, *crypto.DeviceIdentity, id.RoomID) (crypto.VerificationRequestResponse, crypto.VerificationHooks) {
	return func(txnID string, device *crypto.DeviceIdentity, roomID id.RoomID) (crypto.VerificationRequestResponse, crypto.VerificationHooks) {
		if device.UserID != c.mach.Client.UserID {
			return crypto.IgnoreRequest, nil
		} else if roomID != "" {
			return crypto.AcceptRequest, factory(txnID, device)
		}
		c.lock.Lock()
		defer c.lock.Unlock()
		txn, ok := c.txns[txnID]
		switch {
		case !ok:
			hooks := c.wrap(txnID, device, factory(txnID, device))
			c.txns[txnID] = &transaction{device: device, hooks: hooks}
			err := c.send(typeReady, device, &readyContent{
				FromDevice:    c.mach.Client.DeviceID,
				Methods:       []event.VerificationMethod{event.VerificationMethodSAS},
				TransactionID: txnID,
			})
			if err != nil {
				delete(c.txns, txnID)
				go hooks.OnCancel(true, fmt.Sprintf("Failed to send ready: %v", err), event.VerificationCancelByUser)
			} else {
				go c.timeoutAfter(txnID)
			}
			return crypto.IgnoreRequest, nil
		case txn.outgoing && txn.started && device.DeviceID > c.mach.Client.DeviceID:
			// Both sides sent start: the one from the lexicographically smaller device ID wins.
			return crypto.IgnoreRequest, nil
		default:
			txn.started = true
			return crypto.AcceptRequest, txn.hooks
		}
	}
}

// handleEvent watches for the to-device events mautrix v0.11 doesn't know about (ready),
// and for cancels of transactions the machine doesn't know about yet.
func (c *Compat) handleEvent(evt *event.Event) {
	isReady := evt.Type.Type == typeReady.Type
	if !isReady && evt.Type.Type != event.ToDeviceVerificationCancel.Type {
		return
	}
	var content transactionContent
	var ready readyContent
	if err := json.Unmarshal(evt.Content.VeryRaw, &content); err != nil {
		debug.Printf("Failed to parse %s: %v", evt.Type.Type, err)
		return
	}
	_ = json.Unmarshal(evt.Content.VeryRaw, &ready)
	c.lock.Lock()
	txn, ok := c.txns[content.TransactionID]
	valid := ok && !txn.started && evt.Sender == txn.device.UserID &&
		(!isReady || (txn.outgoing && ready.FromDevice == txn.device.DeviceID))
	if valid {
		txn.started = isReady
		if !isReady {
			delete(c.txns, content.TransactionID)
		}
	}
	c.lock.Unlock()
	if !valid {
		return
	} else if isReady {
		go c.startSAS(content.TransactionID, txn)
	} else {
		go txn.hooks.OnCancel(false, content.Reason, content.Code)
	}
}

func (c *Compat) startSAS(txnID string, txn *transaction) {
	_, err := c.mach.NewSASVerificationWith(txn.device, txn.hooks, txnID, c.mach.DefaultSASTimeout)
	if err != nil {
		txn.hooks.OnCancel(true, fmt.Sprintf("Failed to start SAS: %v", err), event.VerificationCancelByUser)
	}
}
