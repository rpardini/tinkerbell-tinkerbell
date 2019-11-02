package grpcserver

import (
	"context"

	"github.com/golang/protobuf/ptypes/empty"
	exec "github.com/packethost/rover/executor"
	pb "github.com/packethost/rover/protos/rover"
)

// GetWorkflowContexts implements rover.GetWorkflowContexts
func (s *server) GetWorkflowContexts(context context.Context, req *pb.WorkflowContextRequest) (*pb.WorkflowContextList, error) {
	return exec.GetWorkflowContexts(context, req)
}

// GetWorkflowActions implements rover.GetWorkflowActions
func (s *server) GetWorkflowActions(context context.Context, req *pb.WorkflowActionsRequest) (*pb.WorkflowActionList, error) {
	return exec.GetWorkflowActions(context, req)
}

// ReportActionStatus implements rover.ReportActionStatus
func (s *server) ReportActionStatus(context context.Context, req *pb.WorkflowActionStatus) (*empty.Empty, error) {
	return exec.ReportActionStatus(context, req, s.db)
}
