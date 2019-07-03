package state

import (
	"context"
	"errors"
	"fmt"

	etcd "github.com/coreos/etcd/clientv3"
)

// etcdRepository is repository based on etc storage
type etcdRepository struct {
	client *etcd.Client
}

// newEtedRepository creates a new repository based on etcd storage
func newEtedRepository(config interface{}) (Repository, error) {
	v, ok := config.(etcd.Config)
	if !ok {
		return nil, fmt.Errorf("config type is not etc.confit")
	}
	cli, err := etcd.New(v)
	if err != nil {
		return nil, fmt.Errorf("create etc client error:%s", err)
	}
	return &etcdRepository{
		client: cli,
	}, nil
}

// Get retrieves value for given key from etcd
func (r *etcdRepository) Get(ctx context.Context, key string) ([]byte, error) {
	resp, err := r.get(ctx, key)
	if err != nil {
		return nil, err
	}
	return r.getValue(key, resp)
}

// Put puts a key-value pair into etcd
func (r *etcdRepository) Put(ctx context.Context, key string, val []byte) error {
	_, err := r.client.Put(ctx, key, string(val))
	if err != nil {
		return err
	}
	return nil
}

// Delete deletes value for given key from etcd
func (r *etcdRepository) Delete(ctx context.Context, key string) error {
	_, err := r.client.Delete(ctx, key)
	if err != nil {
		return err
	}
	return nil
}

// Close closes etcd client
func (r *etcdRepository) Close() error {
	return r.client.Close()
}

// Heartbeat does heartbeat on the key with a value and ttl based on etcd
func (r *etcdRepository) Heartbeat(ctx context.Context, key string, value []byte, ttl int64) (<-chan Closed, error) {
	h := newHeartbeat(r.client, key, value, ttl)
	err := h.grantKeepAliveLease(ctx)
	if err != nil {
		return nil, err
	}
	ch := make(chan Closed)
	// do keepalive/retry background
	go func() {
		// close closed channel, if keep alive stopped
		defer close(ch)
		h.keepAlive(ctx, false)
	}()
	return ch, nil
}

// PutIfNotExitAndKeepLease  puts a key with a value.it will be success
// if the key does not exist,otherwise it will be failed.When this
// operation success,it will do keepalive background
func (r *etcdRepository) PutIfNotExist(ctx context.Context, key string,
	value []byte, ttl int64) (bool, <-chan Closed, error) {
	h := newHeartbeat(r.client, key, value, ttl)
	success, err := h.PutIfNotExist(ctx)
	if err != nil {
		return false, nil, err
	}
	// when put success,do keep alive
	if success {
		ch := make(chan Closed)
		// do keepalive/retry background
		go func() {
			// close closed channel, if keep alive stopped
			defer close(ch)
			h.keepAlive(ctx, true)
		}()
		return success, ch, nil
	}
	return success, nil, nil
}

// Watch watches on a key. The watched events will be returned through the returned channel.
func (r *etcdRepository) Watch(ctx context.Context, key string) (WatchEventChan, error) {
	resp, err := r.get(ctx, key)
	if err != nil {
		return nil, err
	}
	// watch key next revision
	opts := []etcd.OpOption{etcd.WithRev(resp.Header.Revision + 1)}
	wch := r.client.Watch(ctx, key, opts...)

	// make len=1 chan for notify init event if key exist
	ch := make(chan *Event, 1)

	// if key exist notify for got value
	if len(resp.Kvs) != 0 {
		firstkv := resp.Kvs[0]
		if len(firstkv.Value) != 0 {
			if !r.notifyWatchEvent(ctx, ch, &Event{Type: EventTypeModify, Key: key, Value: firstkv.Value}) {
				close(ch)
				return nil, fmt.Errorf("notify watch event error, maybe context is canceled")
			}
		}
	}
	// start goroutine handle watch event in background
	go r.handleWatchEvent(ctx, wch, ch)
	return ch, nil
}

// notifyWatchEvent notify watch event through channel, chan <- event
func (r *etcdRepository) notifyWatchEvent(ctx context.Context, ch chan *Event, event *Event) bool {
	select {
	case ch <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

// handleWatchEvent handles etcd watch event, then convert repository watch event
func (r *etcdRepository) handleWatchEvent(ctx context.Context, wc etcd.WatchChan, ech chan *Event) {
	defer close(ech)
	for watchResp := range wc {
		err := watchResp.Err()
		if err != nil {
			if !r.notifyWatchEvent(ctx, ech, &Event{Err: err}) {
				return
			}
		}
		// convert event
		for _, event := range watchResp.Events {
			eventType := EventTypeModify
			if event.Type == etcd.EventTypeDelete {
				eventType = EventTypeDelete
			}
			kv := event.Kv
			if !r.notifyWatchEvent(ctx, ech, &Event{Type: eventType, Key: string(kv.Key), Value: kv.Value}) {
				return
			}
		}
	}
	r.notifyWatchEvent(ctx, ech, &Event{Err: errors.New("watch is closed")})
}

// get returns response of get operator
func (r *etcdRepository) get(ctx context.Context, key string) (*etcd.GetResponse, error) {
	resp, err := r.client.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("get value failure for key[%s], error:%s", key, err)
	}
	return resp, nil
}

// getValue returns value of get's response
func (r *etcdRepository) getValue(key string, resp *etcd.GetResponse) ([]byte, error) {
	if len(resp.Kvs) == 0 {
		return nil, fmt.Errorf("key[%s] not exist", key)
	}

	firstkv := resp.Kvs[0]
	if len(firstkv.Value) == 0 {
		return nil, fmt.Errorf("key[%s]'s value is empty", key)
	}
	return firstkv.Value, nil
}

// WatchPrefix watches on a prefix.All of the changes who has the prefix
// will be notified through the WatchEventChan channel.
func (r *etcdRepository) WatchPrefix(ctx context.Context, prefixKey string) (WatchEventChan, error) {
	resp, err := r.client.Get(ctx, prefixKey, etcd.WithPrefix(), etcd.WithSerializable())
	if err != nil {
		return nil, err
	}
	//watch the next revision
	options := append([]etcd.OpOption{etcd.WithRev(resp.Header.Revision + 1), etcd.WithPrefix(), etcd.WithPrevKV()})
	wch := r.client.Watch(ctx, prefixKey, options...)

	ch := make(chan *Event)
	// convert  event
	if len(resp.Kvs) != 0 {
		for i := 0; i < len(resp.Kvs); i++ {
			if !r.notifyWatchEvent(ctx, ch, &Event{Type: EventTypeModify,
				Key: string(resp.Kvs[i].Key), Value: resp.Kvs[i].Value}) {
				close(ch)
				return nil, fmt.Errorf("notify watch event error, maybe context is canceled")
			}
		}
	}
	// start goroutine handle watch event in background
	go r.handleWatchEvent(ctx, wch, ch)
	return ch, err
}

// DeleteWithValue deletes the key with the value.it will returns success
// if the value of the key in the etcd equals the incoming value
func (r *etcdRepository) DeleteWithValue(ctx context.Context, key string, value []byte) (bool, error) {
	cmp := etcd.Compare(etcd.Value(key), "=", string(value))
	resp, err := r.client.Txn(ctx).If(cmp).Then(etcd.OpDelete(key)).Commit()
	if err != nil {
		return false, err
	}
	return resp.Succeeded, nil
}
