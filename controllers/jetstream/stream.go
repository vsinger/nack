// Copyright 2020 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package jetstream

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	apis "github.com/nats-io/nack/pkg/jetstream/apis/jetstream/v1"
	typed "github.com/nats-io/nack/pkg/jetstream/generated/clientset/versioned/typed/jetstream/v1"

	k8sapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	k8smeta "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const (
	streamFinalizerKey  = "streamfinalizer.jetstream.nats.io"
	streamReadyCondType = "Ready"
)

func streamEventHandlers(ctx context.Context, q workqueue.RateLimitingInterface, jif typed.JetstreamV1Interface) cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			stream, ok := obj.(*apis.Stream)
			if !ok {
				return
			}

			if err := enqueueStreamWork(q, stream); err != nil {
				utilruntime.HandleError(err)
			}
		},
		UpdateFunc: func(prevObj, nextObj interface{}) {
			prev, ok := prevObj.(*apis.Stream)
			if !ok {
				return
			}

			next, ok := nextObj.(*apis.Stream)
			if !ok {
				return
			}

			if err := validateStreamUpdate(prev, next); errors.Is(err, errNothingToUpdate) {
				return
			} else if err != nil {
				sif := jif.Streams(next.Namespace)
				if _, serr := setStreamErrored(ctx, next, sif, err); serr != nil {
					err = fmt.Errorf("%s: %w", err, serr)
				}

				utilruntime.HandleError(err)
				return
			}

			if err := enqueueStreamWork(q, next); err != nil {
				utilruntime.HandleError(err)
			}
		},
		DeleteFunc: func(obj interface{}) {
			stream, ok := obj.(*apis.Stream)
			if !ok {
				return
			}

			if err := enqueueStreamWork(q, stream); err != nil {
				utilruntime.HandleError(err)
			}
		},
	}
}

func enqueueStreamWork(q workqueue.RateLimitingInterface, stream *apis.Stream) (err error) {
	key, err := cache.MetaNamespaceKeyFunc(stream)
	if err != nil {
		return fmt.Errorf("failed to queue stream work: %w", err)
	}

	q.Add(key)
	return nil
}

func validateStreamUpdate(prev, next *apis.Stream) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("failed to validate update: %w", err)
		}
	}()

	if prev.DeletionTimestamp != next.DeletionTimestamp {
		return nil
	}

	if prev.Spec.Name != next.Spec.Name {
		return fmt.Errorf("updating stream name is not allowed, please recreate")
	}
	if prev.Spec.Storage != next.Spec.Storage {
		return fmt.Errorf("updating stream storage is not allowed, please recreate")
	}

	if equality.Semantic.DeepEqual(prev.Spec, next.Spec) {
		return errNothingToUpdate
	}

	return nil
}

func (c *Controller) runStreamQueue() {
	for {
		c.processNextQueueItem()
	}
}

func (c *Controller) processNextQueueItem() {
	item, shutdown := c.streamQueue.Get()
	if shutdown {
		return
	}
	defer c.streamQueue.Done(item)

	ns, name, err := splitNamespaceName(item)
	if err != nil {
		// Probably junk, clean it up.
		utilruntime.HandleError(err)
		c.streamQueue.Forget(item)
		return
	}

	err = c.processStream(ns, name)
	if err == nil {
		// Item processed successfully, don't requeue.
		c.streamQueue.Forget(item)
		return
	}

	utilruntime.HandleError(err)

	if c.streamQueue.NumRequeues(item) < maxQueueRetries {
		// Failed to process item, try again.
		c.streamQueue.AddRateLimited(item)
		return
	}

	// If we haven't been able to recover by this point, then just stop.
	// The user should have enough info in kubectl describe to debug.
	c.streamQueue.Forget(item)
}

func getNATSOptions(connName string) []nats.Option {
	return []nats.Option{
		nats.Name(connName),
		nats.Option(func(o *nats.Options) error {
			o.Pedantic = true
			return nil

		}),
	}
}

func (c *Controller) processStream(ns, name string) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("failed to process stream: %w", err)
		}
	}()

	stream, err := c.streamLister.Streams(ns).Get(name)
	if err != nil && k8serrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}

	sif := c.ji.Streams(stream.Namespace)

	err = c.sc.Connect(strings.Join(stream.Spec.Servers, ","), getNATSOptions(c.natsName)...)
	if err != nil {
		if _, serr := setStreamErrored(c.ctx, stream, sif, err); serr != nil {
			return fmt.Errorf("%s: %w", err, serr)
		}
		return err
	}
	defer c.sc.Close()

	deleteOK := stream.GetDeletionTimestamp() != nil
	newGeneration := stream.Generation != stream.Status.ObservedGeneration
	streamExists, err := c.sc.Exists(c.ctx, stream.Spec.Name)
	if err != nil {
		if _, serr := setStreamErrored(c.ctx, stream, sif, err); serr != nil {
			return fmt.Errorf("%s: %w", err, serr)
		}
		return err
	}
	updateOK := (streamExists && !deleteOK && newGeneration)
	createOK := (!streamExists && !deleteOK && newGeneration)

	switch {
	case updateOK:
		c.normalEvent(stream, "Updating", fmt.Sprintf("Updating stream %q", stream.Spec.Name))
		if err := c.sc.Update(c.ctx, stream); err != nil {
			if _, serr := setStreamErrored(c.ctx, stream, sif, err); serr != nil {
				return fmt.Errorf("%s: %w", err, serr)
			}
			return err
		}

		res, err := setStreamFinalizer(c.ctx, stream, sif)
		if err != nil {
			if _, serr := setStreamErrored(c.ctx, stream, sif, err); serr != nil {
				return fmt.Errorf("%s: %w", err, serr)
			}
			return err
		}
		stream = res

		if _, err := setStreamSynced(c.ctx, stream, sif); err != nil {
			return err
		}
		c.normalEvent(stream, "Updated", fmt.Sprintf("Updated stream %q", stream.Spec.Name))
		return nil
	case createOK:
		c.normalEvent(stream, "Creating", fmt.Sprintf("Creating stream %q", stream.Spec.Name))
		if err := c.sc.Create(c.ctx, stream); err != nil {
			if _, serr := setStreamErrored(c.ctx, stream, sif, err); serr != nil {
				return fmt.Errorf("%s: %w", err, serr)
			}
			return err
		}

		res, err := setStreamFinalizer(c.ctx, stream, sif)
		if err != nil {
			if _, serr := setStreamErrored(c.ctx, stream, sif, err); serr != nil {
				return fmt.Errorf("%s: %w", err, serr)
			}
			return err
		}
		stream = res

		if _, err := setStreamSynced(c.ctx, stream, sif); err != nil {
			return err
		}
		c.normalEvent(stream, "Created", fmt.Sprintf("Created stream %q", stream.Spec.Name))
		return err
	case deleteOK:
		c.normalEvent(stream, "Deleting", fmt.Sprintf("Deleting stream %q", stream.Spec.Name))
		if err := c.sc.Delete(c.ctx, stream.Spec.Name); err != nil {
			if _, serr := setStreamErrored(c.ctx, stream, sif, err); serr != nil {
				return fmt.Errorf("%s: %w", err, serr)
			}
			return err
		}

		if _, err := clearStreamFinalizer(c.ctx, stream, sif); err != nil {
			if _, serr := setStreamErrored(c.ctx, stream, sif, err); serr != nil {
				return fmt.Errorf("%s: %w", err, serr)
			}
			return err
		}

		return nil
	}

	// default: Nothing to do.
	return nil
}

func setStreamErrored(ctx context.Context, s *apis.Stream, sif typed.StreamInterface, err error) (*apis.Stream, error) {
	if err == nil {
		return s, nil
	}

	sc := s.DeepCopy()
	sc.Status.Conditions = upsertStreamCondition(sc.Status.Conditions, apis.StreamCondition{
		Type:               streamReadyCondType,
		Status:             k8sapi.ConditionFalse,
		LastTransitionTime: time.Now().UTC().Format(time.RFC3339Nano),
		Reason:             "Errored",
		Message:            err.Error(),
	})
	sc.Status.Conditions = pruneStreamConditions(sc.Status.Conditions)

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	res, err := sif.UpdateStatus(ctx, sc, k8smeta.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to set stream errored status: %w", err)
	}

	return res, nil
}

func setStreamSynced(ctx context.Context, s *apis.Stream, i typed.StreamInterface) (*apis.Stream, error) {
	sc := s.DeepCopy()

	sc.Status.ObservedGeneration = s.Generation
	sc.Status.Conditions = upsertStreamCondition(sc.Status.Conditions, apis.StreamCondition{
		Type:               streamReadyCondType,
		Status:             k8sapi.ConditionTrue,
		LastTransitionTime: time.Now().UTC().Format(time.RFC3339Nano),
		Reason:             "Synced",
		Message:            "Stream is synced with spec",
	})
	sc.Status.Conditions = pruneStreamConditions(sc.Status.Conditions)

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	res, err := i.UpdateStatus(ctx, sc, k8smeta.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to set %q stream synced status: %w", s.Spec.Name, err)
	}

	return res, nil
}

func upsertStreamCondition(cs []apis.StreamCondition, next apis.StreamCondition) []apis.StreamCondition {
	for i := 0; i < len(cs); i++ {
		if cs[i].Type != next.Type {
			continue
		}

		cs[i] = next
		return cs
	}

	return append(cs, next)
}

func pruneStreamConditions(cs []apis.StreamCondition) []apis.StreamCondition {
	const maxCond = 10
	if len(cs) < maxCond {
		return cs
	}

	cs = cs[len(cs)-maxCond:]
	return cs
}

func setStreamFinalizer(ctx context.Context, s *apis.Stream, sif typed.StreamInterface) (*apis.Stream, error) {
	fs := s.GetFinalizers()
	if hasFinalizerKey(fs, streamFinalizerKey) {
		return s, nil
	}
	fs = append(fs, streamFinalizerKey)
	s.SetFinalizers(fs)

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	res, err := sif.Update(ctx, s, k8smeta.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to set %q stream finalizers: %w", s.GetName(), err)
	}

	return res, nil
}

func clearStreamFinalizer(ctx context.Context, s *apis.Stream, sif typed.StreamInterface) (*apis.Stream, error) {
	if s.GetDeletionTimestamp() == nil {
		// Already deleted.
		return s, nil
	}

	fs := s.GetFinalizers()
	if !hasFinalizerKey(fs, streamFinalizerKey) {
		return s, nil
	}
	var filtered []string
	for _, f := range fs {
		if f == streamFinalizerKey {
			continue
		}
		filtered = append(filtered, f)
	}
	s.SetFinalizers(filtered)

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	res, err := sif.Update(ctx, s, k8smeta.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to clear %q stream finalizers: %w", s.GetName(), err)
	}

	return res, nil
}