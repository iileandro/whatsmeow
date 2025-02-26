// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"go.mau.fi/libsignal/signalerror"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/libsignal/groups"
	"go.mau.fi/libsignal/protocol"
	"go.mau.fi/libsignal/session"

	"go.mau.fi/whatsmeow/appstate"
	waBinary "go.mau.fi/whatsmeow/binary"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

var pbSerializer = store.SignalProtobufSerializer

func (cli *Client) handleEncryptedMessage(node *waBinary.Node) {
	info, err := cli.parseMessageInfo(node)
	if err != nil {
		cli.Log.Warnf("Failed to parse message: %v", err)
	} else {
		if len(info.PushName) > 0 && info.PushName != "-" {
			go cli.updatePushName(info.Sender, info, info.PushName)
		}
		cli.decryptMessages(info, node)
	}
}

func (cli *Client) parseMessageSource(node *waBinary.Node) (source types.MessageSource, err error) {
	from, ok := node.Attrs["from"].(types.JID)
	if !ok {
		err = fmt.Errorf("didn't find valid `from` attribute in message")
	} else if from.Server == types.GroupServer || from.Server == types.BroadcastServer {
		source.IsGroup = true
		source.Chat = from
		sender, ok := node.Attrs["participant"].(types.JID)
		if !ok {
			err = fmt.Errorf("didn't find valid `participant` attribute in group message")
		} else {
			source.Sender = sender
			if source.Sender.User == cli.Store.ID.User {
				source.IsFromMe = true
			}
		}
		if from.Server == types.BroadcastServer {
			recipient, ok := node.Attrs["recipient"].(types.JID)
			if ok {
				source.BroadcastListOwner = recipient
			}
		}
	} else if from.User == cli.Store.ID.User {
		source.IsFromMe = true
		source.Sender = from
		recipient, ok := node.Attrs["recipient"].(types.JID)
		if !ok {
			source.Chat = from.ToNonAD()
		} else {
			source.Chat = recipient
		}
	} else {
		source.Chat = from.ToNonAD()
		source.Sender = from
	}
	return
}

func (cli *Client) parseMessageInfo(node *waBinary.Node) (*types.MessageInfo, error) {
	var info types.MessageInfo
	var err error
	var ok bool
	info.MessageSource, err = cli.parseMessageSource(node)
	if err != nil {
		return nil, err
	}
	info.ID, ok = node.Attrs["id"].(string)
	if !ok {
		return nil, fmt.Errorf("didn't find valid `id` attribute in message")
	}
	ts, ok := node.Attrs["t"].(string)
	if !ok {
		return nil, fmt.Errorf("didn't find valid `t` (timestamp) attribute in message")
	}
	tsInt, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("didn't find valid `t` (timestamp) attribute in message: %w", err)
	}
	info.Timestamp = time.Unix(tsInt, 0)

	info.PushName, _ = node.Attrs["notify"].(string)
	info.Category, _ = node.Attrs["category"].(string)

	return &info, nil
}

func (cli *Client) decryptMessages(info *types.MessageInfo, node *waBinary.Node) {
	go cli.sendAck(node)
	if len(node.GetChildrenByTag("unavailable")) == len(node.GetChildren()) {
		cli.Log.Warnf("Unavailable message %s from %s", info.ID, info.SourceString())
		go cli.sendRetryReceipt(node, true)
		cli.dispatchEvent(&events.UndecryptableMessage{Info: *info, IsUnavailable: true})
		return
	}
	children := node.GetChildren()
	cli.Log.Debugf("Decrypting %d messages from %s", len(children), info.SourceString())
	handled := false
	for _, child := range children {
		if child.Tag != "enc" {
			continue
		}
		encType, ok := child.Attrs["type"].(string)
		if !ok {
			continue
		}
		var decrypted []byte
		var err error
		if encType == "pkmsg" || encType == "msg" {
			decrypted, err = cli.decryptDM(&child, info.Sender, encType == "pkmsg")
		} else if info.IsGroup && encType == "skmsg" {
			decrypted, err = cli.decryptGroupMsg(&child, info.Sender, info.Chat)
		} else {
			cli.Log.Warnf("Unhandled encrypted message (type %s) from %s", encType, info.SourceString())
			continue
		}
		if err != nil {
			cli.Log.Warnf("Error decrypting message from %s: %v", info.SourceString(), err)
			go cli.sendRetryReceipt(node, false)
			cli.dispatchEvent(&events.UndecryptableMessage{Info: *info, IsUnavailable: false})
			return
		}

		var msg waProto.Message
		err = proto.Unmarshal(decrypted, &msg)
		if err != nil {
			cli.Log.Warnf("Error unmarshaling decrypted message from %s: %v", info.SourceString(), err)
			continue
		}

		cli.handleDecryptedMessage(info, &msg)
		handled = true
	}
	if handled {
		go cli.sendMessageReceipt(info)
	}
}

func (cli *Client) decryptDM(child *waBinary.Node, from types.JID, isPreKey bool) ([]byte, error) {
	content, _ := child.Content.([]byte)

	builder := session.NewBuilderFromSignal(cli.Store, from.SignalAddress(), pbSerializer)
	cipher := session.NewCipher(builder, from.SignalAddress())
	var plaintext []byte
	if isPreKey {
		preKeyMsg, err := protocol.NewPreKeySignalMessageFromBytes(content, pbSerializer.PreKeySignalMessage, pbSerializer.SignalMessage)
		if err != nil {
			return nil, fmt.Errorf("failed to parse prekey message: %w", err)
		}
		plaintext, _, err = cipher.DecryptMessageReturnKey(preKeyMsg)
		if errors.Is(err, signalerror.ErrUntrustedIdentity) {
			cli.Log.Warnf("Got %v error while trying to decrypt prekey message from %s, clearing stored identity and retrying", err, from)
			err = cli.Store.Identities.DeleteIdentity(from.SignalAddress().String())
			if err != nil {
				cli.Log.Warnf("Failed to delete identity of %s from store after decryption error: %v", from, err)
			}
			err = cli.Store.Sessions.DeleteSession(from.SignalAddress().String())
			if err != nil {
				cli.Log.Warnf("Failed to delete session with %s from store after decryption error: %v", from, err)
			}
			cli.dispatchEvent(&events.IdentityChange{JID: from, Timestamp: time.Now(), Implicit: true})
			plaintext, _, err = cipher.DecryptMessageReturnKey(preKeyMsg)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt prekey message: %w", err)
		}
	} else {
		msg, err := protocol.NewSignalMessageFromBytes(content, pbSerializer.SignalMessage)
		if err != nil {
			return nil, fmt.Errorf("failed to parse normal message: %w", err)
		}
		plaintext, err = cipher.Decrypt(msg)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt normal message: %w", err)
		}
	}
	return unpadMessage(plaintext)
}

func (cli *Client) decryptGroupMsg(child *waBinary.Node, from types.JID, chat types.JID) ([]byte, error) {
	content, _ := child.Content.([]byte)

	senderKeyName := protocol.NewSenderKeyName(chat.String(), from.SignalAddress())
	builder := groups.NewGroupSessionBuilder(cli.Store, pbSerializer)
	cipher := groups.NewGroupCipher(builder, senderKeyName, cli.Store)
	msg, err := protocol.NewSenderKeyMessageFromBytes(content, pbSerializer.SenderKeyMessage)
	if err != nil {
		return nil, fmt.Errorf("failed to parse group message: %w", err)
	}
	plaintext, err := cipher.Decrypt(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt group message: %w", err)
	}
	return unpadMessage(plaintext)
}

const checkPadding = true

func isValidPadding(plaintext []byte) bool {
	lastByte := plaintext[len(plaintext)-1]
	expectedPadding := bytes.Repeat([]byte{lastByte}, int(lastByte))
	return bytes.HasSuffix(plaintext, expectedPadding)
}

func unpadMessage(plaintext []byte) ([]byte, error) {
	if checkPadding && !isValidPadding(plaintext) {
		return nil, fmt.Errorf("plaintext doesn't have expected padding")
	}
	return plaintext[:len(plaintext)-int(plaintext[len(plaintext)-1])], nil
}

func padMessage(plaintext []byte) []byte {
	var pad [1]byte
	_, err := rand.Read(pad[:])
	if err != nil {
		panic(err)
	}
	pad[0] &= 0xf
	if pad[0] == 0 {
		pad[0] = 0xf
	}
	plaintext = append(plaintext, bytes.Repeat(pad[:], int(pad[0]))...)
	return plaintext
}

func (cli *Client) handleSenderKeyDistributionMessage(chat, from types.JID, rawSKDMsg *waProto.SenderKeyDistributionMessage) {
	builder := groups.NewGroupSessionBuilder(cli.Store, pbSerializer)
	senderKeyName := protocol.NewSenderKeyName(chat.String(), from.SignalAddress())
	sdkMsg, err := protocol.NewSenderKeyDistributionMessageFromBytes(rawSKDMsg.AxolotlSenderKeyDistributionMessage, pbSerializer.SenderKeyDistributionMessage)
	if err != nil {
		cli.Log.Errorf("Failed to parse sender key distribution message from %s for %s: %v", from, chat, err)
		return
	}
	builder.Process(senderKeyName, sdkMsg)
	cli.Log.Debugf("Processed sender key distribution message from %s in %s", senderKeyName.Sender().String(), senderKeyName.GroupID())
}

func (cli *Client) handleHistorySyncNotification(notif *waProto.HistorySyncNotification) {
	var historySync waProto.HistorySync
	if data, err := cli.Download(notif); err != nil {
		cli.Log.Errorf("Failed to download history sync data: %v", err)
	} else if reader, err := zlib.NewReader(bytes.NewReader(data)); err != nil {
		cli.Log.Errorf("Failed to create zlib reader for history sync data: %v", err)
	} else if rawData, err := io.ReadAll(reader); err != nil {
		cli.Log.Errorf("Failed to decompress history sync data: %v", err)
	} else if err = proto.Unmarshal(rawData, &historySync); err != nil {
		cli.Log.Errorf("Failed to unmarshal history sync data: %v", err)
	} else {
		cli.Log.Debugf("Received history sync")
		if historySync.GetSyncType() == waProto.HistorySync_PUSH_NAME {
			go cli.handleHistoricalPushNames(historySync.GetPushnames())
		}
		cli.dispatchEvent(&events.HistorySync{
			Data: &historySync,
		})
	}
}

func (cli *Client) handleAppStateSyncKeyShare(keys *waProto.AppStateSyncKeyShare) {
	cli.Log.Debugf("Got %d new app state keys", len(keys.GetKeys()))
	for _, key := range keys.GetKeys() {
		marshaledFingerprint, err := proto.Marshal(key.GetKeyData().GetFingerprint())
		if err != nil {
			cli.Log.Errorf("Failed to marshal fingerprint of app state sync key %X", key.GetKeyId().GetKeyId())
			continue
		}
		err = cli.Store.AppStateKeys.PutAppStateSyncKey(key.GetKeyId().GetKeyId(), store.AppStateSyncKey{
			Data:        key.GetKeyData().GetKeyData(),
			Fingerprint: marshaledFingerprint,
			Timestamp:   key.GetKeyData().GetTimestamp(),
		})
		if err != nil {
			cli.Log.Errorf("Failed to store app state sync key %X", key.GetKeyId().GetKeyId())
			continue
		}
		cli.Log.Debugf("Received app state sync key %X", key.GetKeyId().GetKeyId())
	}

	for _, name := range appstate.AllPatchNames {
		err := cli.FetchAppState(name, false, true)
		if err != nil {
			cli.Log.Errorf("Failed to do initial fetch of app state %s: %v", name, err)
		}
	}
}

func (cli *Client) handleProtocolMessage(info *types.MessageInfo, msg *waProto.Message) {
	protoMsg := msg.GetProtocolMessage()

	if protoMsg.GetHistorySyncNotification() != nil && info.IsFromMe {
		cli.handleHistorySyncNotification(protoMsg.HistorySyncNotification)
		cli.sendProtocolMessageReceipt(info.ID, "hist_sync")
	}

	if protoMsg.GetAppStateSyncKeyShare() != nil && info.IsFromMe {
		cli.handleAppStateSyncKeyShare(protoMsg.AppStateSyncKeyShare)
	}

	if info.Category == "peer" {
		cli.sendProtocolMessageReceipt(info.ID, "peer_msg")
	}
}

func (cli *Client) handleDecryptedMessage(info *types.MessageInfo, msg *waProto.Message) {
	evt := &events.Message{Info: *info, RawMessage: msg}

	// First unwrap device sent messages
	if msg.GetDeviceSentMessage().GetMessage() != nil {
		msg = msg.GetDeviceSentMessage().GetMessage()
		evt.Info.DeviceSentMeta = &types.DeviceSentMeta{
			DestinationJID: msg.GetDeviceSentMessage().GetDestinationJid(),
			Phash:          msg.GetDeviceSentMessage().GetPhash(),
		}
	}

	if msg.GetSenderKeyDistributionMessage() != nil {
		if !info.IsGroup {
			cli.Log.Warnf("Got sender key distribution message in non-group chat from", info.Sender)
		} else {
			cli.handleSenderKeyDistributionMessage(info.Chat, info.Sender, msg.SenderKeyDistributionMessage)
		}
	}
	if msg.GetProtocolMessage() != nil {
		go cli.handleProtocolMessage(info, msg)
	}

	// Unwrap ephemeral and view-once messages
	// Hopefully sender key distribution messages and protocol messages can't be inside ephemeral messages
	if msg.GetEphemeralMessage().GetMessage() != nil {
		msg = msg.GetEphemeralMessage().GetMessage()
		evt.IsEphemeral = true
	}
	if msg.GetViewOnceMessage().GetMessage() != nil {
		msg = msg.GetViewOnceMessage().GetMessage()
		evt.IsViewOnce = true
	}
	evt.Message = msg

	cli.dispatchEvent(evt)
}

func (cli *Client) sendProtocolMessageReceipt(id, msgType string) {
	if len(id) == 0 || cli.Store.ID == nil {
		return
	}
	err := cli.sendNode(waBinary.Node{
		Tag: "receipt",
		Attrs: waBinary.Attrs{
			"id":   id,
			"type": msgType,
			"to":   types.NewJID(cli.Store.ID.User, types.LegacyUserServer),
		},
		Content: nil,
	})
	if err != nil {
		cli.Log.Warnf("Failed to send acknowledgement for protocol message %s: %v", id, err)
	}
}
