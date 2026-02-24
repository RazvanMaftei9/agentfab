package message

import (
	"log/slog"
	"time"

	pb "github.com/razvanmaftei/agentfab/gen/agentfab/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// protoMessageType maps Go-native MessageType to proto enum.
var protoMessageType = map[MessageType]pb.MessageType{
	TypeTaskAssignment:     pb.MessageType_MESSAGE_TYPE_TASK_ASSIGNMENT,
	TypeTaskResult:         pb.MessageType_MESSAGE_TYPE_TASK_RESULT,
	TypeEscalation:         pb.MessageType_MESSAGE_TYPE_ESCALATION,
	TypeEscalationResponse: pb.MessageType_MESSAGE_TYPE_ESCALATION_RESPONSE,
	TypeReviewRequest:      pb.MessageType_MESSAGE_TYPE_REVIEW_REQUEST,
	TypeReviewResponse:     pb.MessageType_MESSAGE_TYPE_REVIEW_RESPONSE,
	TypeStatusUpdate:       pb.MessageType_MESSAGE_TYPE_STATUS_UPDATE,
	TypeUserQuery:          pb.MessageType_MESSAGE_TYPE_USER_QUERY,
	TypeUserResponse:       pb.MessageType_MESSAGE_TYPE_USER_RESPONSE,
}

// nativeMessageType maps proto enum to Go-native MessageType.
var nativeMessageType = map[pb.MessageType]MessageType{
	pb.MessageType_MESSAGE_TYPE_TASK_ASSIGNMENT:     TypeTaskAssignment,
	pb.MessageType_MESSAGE_TYPE_TASK_RESULT:         TypeTaskResult,
	pb.MessageType_MESSAGE_TYPE_ESCALATION:          TypeEscalation,
	pb.MessageType_MESSAGE_TYPE_ESCALATION_RESPONSE: TypeEscalationResponse,
	pb.MessageType_MESSAGE_TYPE_REVIEW_REQUEST:      TypeReviewRequest,
	pb.MessageType_MESSAGE_TYPE_REVIEW_RESPONSE:     TypeReviewResponse,
	pb.MessageType_MESSAGE_TYPE_STATUS_UPDATE:       TypeStatusUpdate,
	pb.MessageType_MESSAGE_TYPE_USER_QUERY:          TypeUserQuery,
	pb.MessageType_MESSAGE_TYPE_USER_RESPONSE:       TypeUserResponse,
}

// ToProto converts a Go-native Message to a proto Message.
func ToProto(msg *Message) *pb.Message {
	if msg == nil {
		return nil
	}

	pbMsg := &pb.Message{
		Id:        msg.ID,
		RequestId: msg.RequestID,
		From:      msg.From,
		To:        msg.To,
		Type:      protoMessageType[msg.Type],
		Metadata:  msg.Metadata,
		Timestamp: msg.Timestamp.UnixNano(),
	}

	for _, p := range msg.Parts {
		pbMsg.Parts = append(pbMsg.Parts, partToProto(p))
	}

	if msg.TokenUsage != nil {
		pbMsg.TokenUsage = &pb.TokenUsage{
			InputTokens:     msg.TokenUsage.InputTokens,
			OutputTokens:    msg.TokenUsage.OutputTokens,
			TotalTokens:     msg.TokenUsage.TotalTokens,
			TotalCalls:      msg.TokenUsage.TotalCalls,
			CacheReadTokens: msg.TokenUsage.CacheReadTokens,
			Model:           msg.TokenUsage.Model,
		}
	}

	return pbMsg
}

// FromProto converts a proto Message to a Go-native Message.
func FromProto(pbMsg *pb.Message) *Message {
	if pbMsg == nil {
		return nil
	}

	msg := &Message{
		ID:        pbMsg.Id,
		RequestID: pbMsg.RequestId,
		From:      pbMsg.From,
		To:        pbMsg.To,
		Type:      nativeMessageType[pbMsg.Type],
		Metadata:  pbMsg.Metadata,
		Timestamp: time.Unix(0, pbMsg.Timestamp),
	}

	for _, p := range pbMsg.Parts {
		msg.Parts = append(msg.Parts, protoToPart(p))
	}

	if pbMsg.TokenUsage != nil {
		msg.TokenUsage = &TokenUsage{
			InputTokens:     pbMsg.TokenUsage.InputTokens,
			OutputTokens:    pbMsg.TokenUsage.OutputTokens,
			TotalTokens:     pbMsg.TokenUsage.TotalTokens,
			TotalCalls:      pbMsg.TokenUsage.TotalCalls,
			CacheReadTokens: pbMsg.TokenUsage.CacheReadTokens,
			Model:           pbMsg.TokenUsage.Model,
		}
	}

	return msg
}

func partToProto(p Part) *pb.Part {
	switch v := p.(type) {
	case TextPart:
		return &pb.Part{Part: &pb.Part_Text{Text: &pb.TextPart{Text: v.Text}}}
	case FilePart:
		return &pb.Part{Part: &pb.Part_File{File: &pb.FilePart{
			Uri:      v.URI,
			MimeType: v.MimeType,
			Name:     v.Name,
		}}}
	case DataPart:
		s, err := structpb.NewStruct(v.Data)
		if err != nil {
			slog.Warn("failed to convert DataPart to proto struct", "error", err)
			return &pb.Part{}
		}
		return &pb.Part{Part: &pb.Part_Data{Data: &pb.DataPart{Data: s}}}
	default:
		return &pb.Part{}
	}
}

func protoToPart(p *pb.Part) Part {
	switch v := p.Part.(type) {
	case *pb.Part_Text:
		return TextPart{Text: v.Text.Text}
	case *pb.Part_File:
		return FilePart{URI: v.File.Uri, MimeType: v.File.MimeType, Name: v.File.Name}
	case *pb.Part_Data:
		if v.Data.Data != nil {
			return DataPart{Data: v.Data.Data.AsMap()}
		}
		return DataPart{Data: map[string]any{}}
	default:
		return TextPart{}
	}
}
