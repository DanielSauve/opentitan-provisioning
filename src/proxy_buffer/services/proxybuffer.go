// Copyright lowRISC contributors (OpenTitan project).
// Licensed under the Apache License, Version 2.0, see LICENSE for details.
// SPDX-License-Identifier: Apache-2.0

// Package proxybuffer implements the gRPC ProxyBufferService interface.
package proxybuffer

import (
	"context"
	"log"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pbp "github.com/lowRISC/opentitan-provisioning/src/proxy_buffer/proto/proxy_buffer_go_pb"
	"github.com/lowRISC/opentitan-provisioning/src/proxy_buffer/proto/validators"
	"github.com/lowRISC/opentitan-provisioning/src/proxy_buffer/store/db"
)

// Every registry service frontend must implement the `RegistryDevice` function.
type Registry interface {
	RegisterDevice(ctx context.Context, request *pbp.DeviceRegistrationRequest, opts ...grpc.CallOption) (*pbp.DeviceRegistrationResponse, error)
}

// server is the server object.
type server struct {
	db *db.DB
}

// NewProxyBufferServer returns an implementation of the ProxyBufferService
// gRPC server.
func NewProxyBufferServer(db *db.DB) pbp.ProxyBufferServiceServer {
	return &server{db: db}
}

// RegisterDevice registers a new device record.
//
// Validates request and then durably records it (locally).
func (s *server) RegisterDevice(ctx context.Context, request *pbp.DeviceRegistrationRequest) (*pbp.DeviceRegistrationResponse, error) {
	device_id := request.Record.DeviceId
	log.Printf("Received device-registration request with DeviceID: %s", device_id)

	response := &pbp.DeviceRegistrationResponse{
		DeviceId: device_id,
	}

	if err := validators.ValidateDeviceRegistrationRequest(request); err != nil {
		response.Status = pbp.DeviceRegistrationStatus_DEVICE_REGISTRATION_STATUS_BAD_REQUEST
		return response, status.Errorf(codes.InvalidArgument, "failed request validation: %v", err)
	}

	if err := s.db.InsertDevice(ctx, request.Record); err != nil {
		// E.g. The given device is still in the buffer but its DeviceData has changed.
		response.Status = pbp.DeviceRegistrationStatus_DEVICE_REGISTRATION_STATUS_BAD_REQUEST
		return response, status.Errorf(codes.Internal, "failed to insert record: %v", err)
	}

	response.Status = pbp.DeviceRegistrationStatus_DEVICE_REGISTRATION_STATUS_SUCCESS
	return response, nil
}
