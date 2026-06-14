// Package push delivers Web Push (VAPID) notifications and persists browser
// PushSubscriptions keyed by uid.
package push

import (
	"encoding/json"
	"os"
	"sync"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// Store holds push subscriptions and sends notifications.
type Store struct {
	mu     sync.Mutex
	path   string
	subs   map[string][]webpush.Subscription // uid -> subscriptions
	priv   string
	pub    string
	mailto string
}

// NewStore loads (or starts empty) the on-disk subscription store and wires the
// VAPID keys used for sending.
func NewStore(path, vapidPriv, vapidPub, mailto string) *Store {
	s := &Store{
		path:   path,
		subs:   make(map[string][]webpush.Subscription),
		priv:   vapidPriv,
		pub:    vapidPub,
		mailto: mailto,
	}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &s.subs)
	}
	return s
}

// PublicKey returns the VAPID public key (application server key) for the browser.
func (s *Store) PublicKey() string { return s.pub }

// Subscribe stores a browser subscription for a uid, de-duplicated by endpoint.
func (s *Store) Subscribe(uid string, sub webpush.Subscription) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.subs[uid] {
		if e.Endpoint == sub.Endpoint {
			return
		}
	}
	s.subs[uid] = append(s.subs[uid], sub)
	s.save()
}

// Send pushes a JSON payload to all of a uid's subscriptions, pruning any that
// the push service reports as gone (404/410).
func (s *Store) Send(uid string, payload []byte) {
	s.mu.Lock()
	subs := append([]webpush.Subscription(nil), s.subs[uid]...)
	s.mu.Unlock()

	var dead map[string]bool
	for i := range subs {
		sub := subs[i]
		resp, err := webpush.SendNotification(payload, &sub, &webpush.Options{
			Subscriber:      s.mailto,
			VAPIDPublicKey:  s.pub,
			VAPIDPrivateKey: s.priv,
			TTL:             30,
		})
		if err != nil {
			continue
		}
		code := resp.StatusCode
		_ = resp.Body.Close()
		if code == 404 || code == 410 {
			if dead == nil {
				dead = map[string]bool{}
			}
			dead[sub.Endpoint] = true
		}
	}
	if len(dead) > 0 {
		s.prune(uid, dead)
	}
}

func (s *Store) prune(uid string, dead map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.subs[uid][:0]
	for _, e := range s.subs[uid] {
		if !dead[e.Endpoint] {
			kept = append(kept, e)
		}
	}
	if len(kept) == 0 {
		delete(s.subs, uid)
	} else {
		s.subs[uid] = kept
	}
	s.save()
}

// save writes the store atomically. Caller holds the lock.
func (s *Store) save() {
	data, err := json.Marshal(s.subs)
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}
