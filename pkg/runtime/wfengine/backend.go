/*
Copyright 2022 The Dapr Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package wfengine

import (
	"context"
	"errors"
	"fmt"

	"github.com/microsoft/durabletask-go/api"
	"github.com/microsoft/durabletask-go/backend"

	"github.com/dapr/dapr/pkg/actors"
	invokev1 "github.com/dapr/dapr/pkg/messaging/v1"
)

// workflowScheduler is an interface for pushing work items into the backend
type workflowScheduler interface {
	ScheduleWorkflow(wi *backend.OrchestrationWorkItem)
	ScheduleActivity(wi *backend.ActivityWorkItem)
}

type actorBackend struct {
	actors                    actors.Actors
	orchestrationWorkItemChan chan *backend.OrchestrationWorkItem
	activityWorkItemChan      chan *backend.ActivityWorkItem
}

func NewActorBackend() *actorBackend {
	return &actorBackend{
		orchestrationWorkItemChan: make(chan *backend.OrchestrationWorkItem),
		activityWorkItemChan:      make(chan *backend.ActivityWorkItem),
	}
}

func (be *actorBackend) SetActorRuntime(actors actors.Actors) {
	be.actors = actors
}

// ScheduleActivity implements workflowScheduler
func (be *actorBackend) ScheduleActivity(wi *backend.ActivityWorkItem) {
	be.activityWorkItemChan <- wi
}

// ScheduleWorkflow implements workflowScheduler
func (be *actorBackend) ScheduleWorkflow(wi *backend.OrchestrationWorkItem) {
	be.orchestrationWorkItemChan <- wi
}

// CreateOrchestrationInstance implements backend.Backend and creates a new workflow instance.
//
// Internally, creating a workflow instance also creates a new actor with the same ID. The create
// request is saved into the actor's "inbox" and then executed via a reminder thread. If the app is
// scaled out across multiple replicas, the actor might get assigned to a replicas other than this one.
func (be *actorBackend) CreateOrchestrationInstance(ctx context.Context, e *backend.HistoryEvent) error {
	if err := be.validateConfiguration(); err != nil {
		return err
	}

	var workflowInstanceID string
	if es := e.GetExecutionStarted(); es == nil {
		return errors.New("the history event must be an ExecutionStartedEvent")
	} else if oi := es.GetOrchestrationInstance(); oi == nil {
		return errors.New("the ExecutionStartedEvent did not contain orchestration instance information")
	} else {
		workflowInstanceID = oi.GetInstanceId()
	}

	eventData, err := backend.MarshalHistoryEvent(e)
	if err != nil {
		return err
	}

	// Invoke the well-known workflow actor directly, which will be created by this invocation
	// request. Note that this request goes directly to the actor runtime, bypassing the API layer.
	req := invokev1.
		NewInvokeMethodRequest(CreateWorkflowInstanceMethod).
		WithActor(WorkflowActorType, workflowInstanceID).
		WithRawData(eventData, invokev1.OctetStreamContentType)
	if _, err := be.actors.Call(ctx, req); err != nil {
		return err
	}
	return nil
}

// GetOrchestrationMetadata implements backend.Backend
func (be *actorBackend) GetOrchestrationMetadata(ctx context.Context, id api.InstanceID) (*api.OrchestrationMetadata, error) {
	// Invoke the corresponding actor, which internally stores its own workflow metadata
	req := invokev1.
		NewInvokeMethodRequest(GetWorkflowMetadataMethod).
		WithActor(WorkflowActorType, string(id)).
		WithRawData(nil, invokev1.OctetStreamContentType)
	if res, err := be.actors.Call(ctx, req); err != nil {
		return nil, err
	} else {
		_, data := res.RawData()
		if len(data) == 0 {
			return nil, api.ErrInstanceNotFound
		}
		var metadata api.OrchestrationMetadata
		if err := actors.DecodeInternalActorResponse(data, &metadata); err != nil {
			return nil, fmt.Errorf("failed to decode the internal actor response: %w", err)
		}
		return &metadata, nil
	}
}

// AbandonActivityWorkItem implements backend.Backend
func (*actorBackend) AbandonActivityWorkItem(context.Context, *backend.ActivityWorkItem) error {
	panic("unimplemented")
}

// AbandonOrchestrationWorkItem implements backend.Backend
func (*actorBackend) AbandonOrchestrationWorkItem(context.Context, *backend.OrchestrationWorkItem) error {
	panic("unimplemented")
}

// AddNewOrchestrationEvent implements backend.Backend
func (*actorBackend) AddNewOrchestrationEvent(context.Context, api.InstanceID, *backend.HistoryEvent) error {
	panic("unimplemented")
}

// CompleteActivityWorkItem implements backend.Backend
func (*actorBackend) CompleteActivityWorkItem(ctx context.Context, wi *backend.ActivityWorkItem) error {
	// Resumes workflow execution code path in the actor
	wi.Properties[CallbackChannelProperty].(chan bool) <- true
	return nil
}

// CompleteOrchestrationWorkItem implements backend.Backend
func (*actorBackend) CompleteOrchestrationWorkItem(ctx context.Context, wi *backend.OrchestrationWorkItem) error {
	// Resumes workflow execution code path in the actor
	wi.Properties[CallbackChannelProperty].(chan bool) <- true
	return nil
}

// CreateTaskHub implements backend.Backend
func (*actorBackend) CreateTaskHub(context.Context) error {
	return nil
}

// DeleteTaskHub implements backend.Backend
func (*actorBackend) DeleteTaskHub(context.Context) error {
	panic("unimplemented")
}

// GetActivityWorkItem implements backend.Backend
func (be *actorBackend) GetActivityWorkItem(ctx context.Context) (*backend.ActivityWorkItem, error) {
	// Wait for the workflow actor to signal us with some work to do
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case wi := <-be.activityWorkItemChan:
		return wi, nil
	}
}

// GetOrchestrationRuntimeState implements backend.Backend
func (*actorBackend) GetOrchestrationRuntimeState(context.Context, *backend.OrchestrationWorkItem) (*backend.OrchestrationRuntimeState, error) {
	panic("unimplemented")
}

// GetOrchestrationWorkItem implements backend.Backend
func (be *actorBackend) GetOrchestrationWorkItem(ctx context.Context) (*backend.OrchestrationWorkItem, error) {
	// Wait for the workflow actor to signal us with some work to do
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case wi := <-be.orchestrationWorkItemChan:
		return wi, nil
	}
}

// Start implements backend.Backend
func (be *actorBackend) Start(context.Context) error {
	return be.validateConfiguration()
}

// Stop implements backend.Backend
func (*actorBackend) Stop(context.Context) error {
	return nil
}

// String displays the type information
func (be *actorBackend) String() string {
	return fmt.Sprintf("dapr.actors/v1-alpha")
}

func (be *actorBackend) validateConfiguration() error {
	if be.actors == nil {
		return errors.New("actor runtime has not been configured")
	}
	return nil
}