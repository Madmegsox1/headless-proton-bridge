// Copyright (c) 2022 Proton AG
//
// This file is part of Proton Mail Bridge.
//
// Proton Mail Bridge is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Proton Mail Bridge is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with Proton Mail Bridge.  If not, see <https://www.gnu.org/licenses/>.

package user

import (
	"context"
	"fmt"

	"github.com/ProtonMail/gluon/imap"
	"github.com/ProtonMail/gluon/queue"
	"github.com/ProtonMail/gopenpgp/v2/crypto"
	"github.com/ProtonMail/proton-bridge/v2/internal/events"
	"github.com/ProtonMail/proton-bridge/v2/internal/logging"
	"github.com/ProtonMail/proton-bridge/v2/internal/safe"
	"github.com/ProtonMail/proton-bridge/v2/internal/vault"
	"github.com/bradenaw/juniper/xslices"
	"github.com/sirupsen/logrus"
	"gitlab.protontech.ch/go/liteapi"
)

// handleAPIEvent handles the given liteapi.Event.
func (user *User) handleAPIEvent(ctx context.Context, event liteapi.Event) error {
	if event.User != nil {
		if err := user.handleUserEvent(ctx, *event.User); err != nil {
			return err
		}
	}

	if len(event.Addresses) > 0 {
		if err := user.handleAddressEvents(ctx, event.Addresses); err != nil {
			return err
		}
	}

	if len(event.Labels) > 0 {
		if err := user.handleLabelEvents(ctx, event.Labels); err != nil {
			return err
		}
	}

	if len(event.Messages) > 0 {
		if err := user.handleMessageEvents(ctx, event.Messages); err != nil {
			return err
		}
	}

	return nil
}

// handleUserEvent handles the given user event.
func (user *User) handleUserEvent(_ context.Context, userEvent liteapi.User) error {
	return safe.LockRet(func() error {
		user.log.WithFields(logrus.Fields{
			"userID":   userEvent.ID,
			"username": logging.Sensitive(userEvent.Name),
		}).Info("Handling user event")

		user.apiUser = userEvent

		user.eventCh.Enqueue(events.UserChanged{
			UserID: user.ID(),
		})

		return nil
	}, user.apiUserLock)
}

// handleAddressEvents handles the given address events.
// GODT-1945: If split address mode, need to signal back to bridge to update the addresses.
func (user *User) handleAddressEvents(ctx context.Context, addressEvents []liteapi.AddressEvent) error {
	for _, event := range addressEvents {
		switch event.Action {
		case liteapi.EventCreate:
			if err := user.handleCreateAddressEvent(ctx, event); err != nil {
				return fmt.Errorf("failed to handle create address event: %w", err)
			}

		case liteapi.EventUpdate, liteapi.EventUpdateFlags:
			if err := user.handleUpdateAddressEvent(ctx, event); err != nil {
				return fmt.Errorf("failed to handle update address event: %w", err)
			}

		case liteapi.EventDelete:
			if err := user.handleDeleteAddressEvent(ctx, event); err != nil {
				return fmt.Errorf("failed to delete address: %w", err)
			}
		}
	}

	return nil
}

func (user *User) handleCreateAddressEvent(ctx context.Context, event liteapi.AddressEvent) error {
	if err := safe.LockRet(func() error {
		user.log.WithFields(logrus.Fields{
			"addressID": event.ID,
			"email":     logging.Sensitive(event.Address.Email),
		}).Info("Handling address created event")

		if _, ok := user.apiAddrs[event.Address.ID]; ok {
			return fmt.Errorf("address %q already exists", event.ID)
		}

		user.apiAddrs[event.Address.ID] = event.Address

		switch user.vault.AddressMode() {
		case vault.CombinedMode:
			primAddr, err := getAddrIdx(user.apiAddrs, 0)
			if err != nil {
				return fmt.Errorf("failed to get primary address: %w", err)
			}

			user.updateCh[event.Address.ID] = user.updateCh[primAddr.ID]

		case vault.SplitMode:
			user.updateCh[event.Address.ID] = queue.NewQueuedChannel[imap.Update](0, 0)
		}

		return nil
	}, user.apiAddrsLock, user.updateChLock); err != nil {
		return fmt.Errorf("failed to handle create address event: %w", err)
	}

	user.eventCh.Enqueue(events.UserAddressCreated{
		UserID:    user.ID(),
		AddressID: event.Address.ID,
		Email:     event.Address.Email,
	})

	return safe.RLockRet(func() error {
		if user.vault.AddressMode() == vault.SplitMode {
			if err := syncLabels(ctx, user.client, user.updateCh[event.Address.ID]); err != nil {
				return fmt.Errorf("failed to sync labels to new address: %w", err)
			}
		}

		return nil
	}, user.apiAddrsLock, user.updateChLock)
}

func (user *User) handleUpdateAddressEvent(_ context.Context, event liteapi.AddressEvent) error { //nolint:unparam
	return safe.LockRet(func() error {
		user.log.WithFields(logrus.Fields{
			"addressID": event.ID,
			"email":     logging.Sensitive(event.Address.Email),
		}).Info("Handling address updated event")

		if _, ok := user.apiAddrs[event.Address.ID]; !ok {
			return fmt.Errorf("address %q does not exist", event.Address.ID)
		}

		user.apiAddrs[event.Address.ID] = event.Address

		user.eventCh.Enqueue(events.UserAddressUpdated{
			UserID:    user.ID(),
			AddressID: event.Address.ID,
			Email:     event.Address.Email,
		})

		return nil
	}, user.apiAddrsLock)
}

func (user *User) handleDeleteAddressEvent(_ context.Context, event liteapi.AddressEvent) error {
	return safe.LockRet(func() error {
		user.log.WithField("addressID", event.ID).Info("Handling address deleted event")

		addr, ok := user.apiAddrs[event.ID]
		if !ok {
			return fmt.Errorf("address %q does not exist", event.ID)
		}

		if user.vault.AddressMode() == vault.SplitMode {
			user.updateCh[event.ID].CloseAndDiscardQueued()
			delete(user.updateCh, event.ID)
		}

		delete(user.apiAddrs, event.ID)

		user.eventCh.Enqueue(events.UserAddressDeleted{
			UserID:    user.ID(),
			AddressID: event.ID,
			Email:     addr.Email,
		})

		return nil
	}, user.apiAddrsLock, user.updateChLock)
}

// handleLabelEvents handles the given label events.
func (user *User) handleLabelEvents(ctx context.Context, labelEvents []liteapi.LabelEvent) error {
	for _, event := range labelEvents {
		switch event.Action {
		case liteapi.EventCreate:
			if err := user.handleCreateLabelEvent(ctx, event); err != nil {
				return fmt.Errorf("failed to handle create label event: %w", err)
			}

		case liteapi.EventUpdate, liteapi.EventUpdateFlags:
			if err := user.handleUpdateLabelEvent(ctx, event); err != nil {
				return fmt.Errorf("failed to handle update label event: %w", err)
			}

		case liteapi.EventDelete:
			if err := user.handleDeleteLabelEvent(ctx, event); err != nil {
				return fmt.Errorf("failed to handle delete label event: %w", err)
			}
		}
	}

	return nil
}

func (user *User) handleCreateLabelEvent(_ context.Context, event liteapi.LabelEvent) error { //nolint:unparam
	return safe.LockRet(func() error {
		user.log.WithFields(logrus.Fields{
			"labelID": event.ID,
			"name":    logging.Sensitive(event.Label.Name),
		}).Info("Handling label created event")

		if _, ok := user.apiLabels[event.Label.ID]; ok {
			return fmt.Errorf("label %q already exists", event.ID)
		}

		user.apiLabels[event.Label.ID] = event.Label

		for _, updateCh := range user.updateCh {
			updateCh.Enqueue(newMailboxCreatedUpdate(imap.MailboxID(event.ID), getMailboxName(event.Label)))
		}

		user.eventCh.Enqueue(events.UserLabelCreated{
			UserID:  user.ID(),
			LabelID: event.Label.ID,
			Name:    event.Label.Name,
		})

		return nil
	}, user.apiLabelsLock, user.updateChLock)
}

func (user *User) handleUpdateLabelEvent(_ context.Context, event liteapi.LabelEvent) error { //nolint:unparam
	return safe.LockRet(func() error {
		user.log.WithFields(logrus.Fields{
			"labelID": event.ID,
			"name":    logging.Sensitive(event.Label.Name),
		}).Info("Handling label updated event")

		if _, ok := user.apiLabels[event.Label.ID]; !ok {
			return fmt.Errorf("label %q does not exist", event.ID)
		}

		user.apiLabels[event.Label.ID] = event.Label

		for _, updateCh := range user.updateCh {
			updateCh.Enqueue(imap.NewMailboxUpdated(imap.MailboxID(event.ID), getMailboxName(event.Label)))
		}

		user.eventCh.Enqueue(events.UserLabelUpdated{
			UserID:  user.ID(),
			LabelID: event.Label.ID,
			Name:    event.Label.Name,
		})

		return nil
	}, user.apiLabelsLock, user.updateChLock)
}

func (user *User) handleDeleteLabelEvent(_ context.Context, event liteapi.LabelEvent) error { //nolint:unparam
	return safe.LockRet(func() error {
		user.log.WithField("labelID", event.ID).Info("Handling label deleted event")

		label, ok := user.apiLabels[event.ID]
		if !ok {
			return fmt.Errorf("label %q does not exist", event.ID)
		}

		delete(user.apiLabels, event.ID)

		for _, updateCh := range user.updateCh {
			updateCh.Enqueue(imap.NewMailboxDeleted(imap.MailboxID(event.ID)))
		}

		user.eventCh.Enqueue(events.UserLabelDeleted{
			UserID:  user.ID(),
			LabelID: event.ID,
			Name:    label.Name,
		})

		return nil
	}, user.apiLabelsLock, user.updateChLock)
}

// handleMessageEvents handles the given message events.
func (user *User) handleMessageEvents(ctx context.Context, messageEvents []liteapi.MessageEvent) error {
	for _, event := range messageEvents {
		switch event.Action {
		case liteapi.EventCreate:
			if err := user.handleCreateMessageEvent(ctx, event); err != nil {
				return fmt.Errorf("failed to handle create message event: %w", err)
			}

		case liteapi.EventUpdate, liteapi.EventUpdateFlags:
			if err := user.handleUpdateMessageEvent(ctx, event); err != nil {
				return fmt.Errorf("failed to handle update message event: %w", err)
			}

		case liteapi.EventDelete:
			if err := user.handleDeleteMessageEvent(ctx, event); err != nil {
				return fmt.Errorf("failed to handle delete message event: %w", err)
			}
		}
	}

	return nil
}

func (user *User) handleCreateMessageEvent(ctx context.Context, event liteapi.MessageEvent) error {
	full, err := user.client.GetFullMessage(ctx, event.Message.ID)
	if err != nil {
		return fmt.Errorf("failed to get full message: %w", err)
	}

	return safe.RLockRet(func() error {
		user.log.WithFields(logrus.Fields{
			"messageID": event.ID,
			"subject":   logging.Sensitive(event.Message.Subject),
		}).Info("Handling message created event")

		return withAddrKR(user.apiUser, user.apiAddrs[event.Message.AddressID], user.vault.KeyPass(), func(_, addrKR *crypto.KeyRing) error {
			buildRes, err := buildRFC822(full, addrKR)
			if err != nil {
				return fmt.Errorf("failed to build RFC822 message: %w", err)
			}

			user.updateCh[full.AddressID].Enqueue(imap.NewMessagesCreated(buildRes.update))

			return nil
		})
	}, user.apiUserLock, user.apiAddrsLock, user.updateChLock)
}

func (user *User) handleUpdateMessageEvent(_ context.Context, event liteapi.MessageEvent) error { //nolint:unparam
	return safe.RLockRet(func() error {
		user.log.WithFields(logrus.Fields{
			"messageID": event.ID,
			"subject":   logging.Sensitive(event.Message.Subject),
		}).Info("Handling message updated event")

		update := imap.NewMessageMailboxesUpdated(
			imap.MessageID(event.ID),
			mapTo[string, imap.MailboxID](xslices.Filter(event.Message.LabelIDs, wantLabelID)),
			event.Message.Seen(),
			event.Message.Starred(),
		)

		user.updateCh[event.Message.AddressID].Enqueue(update)

		return nil
	}, user.updateChLock)
}

func (user *User) handleDeleteMessageEvent(_ context.Context, event liteapi.MessageEvent) error { //nolint:unparam
	return safe.RLockRet(func() error {
		user.log.WithField("messageID", event.ID).Info("Handling message deleted event")

		for _, updateCh := range user.updateCh {
			update := imap.NewMessagesDeleted(
				imap.MessageID(event.ID),
			)

			updateCh.Enqueue(update)
		}

		return nil
	}, user.updateChLock)
}

func getMailboxName(label liteapi.Label) []string {
	var name []string

	switch label.Type {
	case liteapi.LabelTypeFolder:
		name = append([]string{folderPrefix}, label.Path...)

	case liteapi.LabelTypeLabel:
		name = append([]string{labelPrefix}, label.Path...)

	case liteapi.LabelTypeContactGroup:
		fallthrough
	case liteapi.LabelTypeSystem:
		fallthrough
	default:
		name = label.Path
	}

	return name
}
