package files

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
)

type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

func caller(ctx context.Context) (uuid.UUID, bool) {
	session, ok := auth.SessionFrom(ctx)
	if !ok {
		return uuid.Nil, false
	}
	return session.User.ID, true
}

func (h *Handler) UploadAttachment(ctx context.Context, req api.UploadAttachmentRequestObject) (api.UploadAttachmentResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.UploadAttachment401JSONResponse(api.Error(unauthorized())), nil
	}
	if req.Body == nil {
		return api.UploadAttachment400JSONResponse{ErrorJSONResponse: badRequest("a file is required")}, nil
	}

	part, err := req.Body.NextPart()
	if err != nil {
		return api.UploadAttachment400JSONResponse{ErrorJSONResponse: badRequest("a file is required")}, nil
	}
	defer func() { _ = part.Close() }()
	if part.FormName() != "file" {
		return api.UploadAttachment400JSONResponse{ErrorJSONResponse: badRequest(`the form field must be named "file"`)}, nil
	}

	attachment, err := h.svc.Upload(ctx, req.ChannelId, userID, part.FileName(), part, req.Params.ExpiresAt)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.UploadAttachment404JSONResponse(api.Error(notFound())), nil
	case errors.Is(err, ErrAttachNotAllowed):
		return api.UploadAttachment403JSONResponse(api.Error(forbidden("this server does not let members attach files"))), nil
	case errors.Is(err, ErrForbidden):
		return api.UploadAttachment403JSONResponse(api.Error(forbidden("you cannot write in this channel"))), nil
	case errors.Is(err, ErrTooLarge):
		return api.UploadAttachment413JSONResponse(api.Error(tooLarge(h.svc.MaxBytes()))), nil
	case errors.Is(err, ErrUnsupportedType):
		return api.UploadAttachment415JSONResponse(api.Error(unsupported())), nil
	case errors.Is(err, ErrEmpty):
		return api.UploadAttachment400JSONResponse{ErrorJSONResponse: badRequest("the file is empty")}, nil
	case err != nil:
		h.log.ErrorContext(ctx, "upload attachment", slog.Any("error", err))
		return api.UploadAttachment500JSONResponse(api.Error(internal("could not store the file"))), nil
	}
	return api.UploadAttachment201JSONResponse(APIAttachment(attachment)), nil
}

func (h *Handler) GetAttachmentLink(ctx context.Context, req api.GetAttachmentLinkRequestObject) (api.GetAttachmentLinkResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.GetAttachmentLink401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	link, err := h.svc.Link(ctx, req.AttachmentId, userID)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrForbidden):
		return api.GetAttachmentLink404JSONResponse(api.Error(notFound())), nil
	case err != nil:
		h.log.ErrorContext(ctx, "sign attachment link", slog.Any("error", err))
		return api.GetAttachmentLink500JSONResponse(api.Error(internal("could not prepare the download"))), nil
	}

	return api.GetAttachmentLink200JSONResponse{
		Url:        link.URL,
		ExpiresAt:  link.ExpiresAt,
		Attachment: APIAttachment(link.Attachment),
	}, nil
}

// APIAttachment is the shape an attachment takes on the wire, for this module
// and for the messages that carry it.
func APIAttachment(a Attachment) api.Attachment {
	attachment := api.Attachment{
		Id:        a.ID,
		FileName:  a.FileName,
		MimeType:  a.MimeType,
		SizeBytes: a.SizeBytes,
		CreatedAt: a.CreatedAt,
		ExpiresAt: a.ExpiresAt,
	}
	if a.Width != nil {
		width := *a.Width
		attachment.Width = &width
	}
	if a.Height != nil {
		height := *a.Height
		attachment.Height = &height
	}
	return attachment
}

func unauthorized() api.ErrorJSONResponse {
	return errorBody(httpx.CodeUnauthorized, "sign in first")
}

func notFound() api.ErrorJSONResponse {
	return errorBody(httpx.CodeNotFound, "not found")
}

func forbidden(message string) api.ErrorJSONResponse {
	return errorBody(httpx.CodeForbidden, message)
}

func tooLarge(limit int64) api.ErrorJSONResponse {
	return api.ErrorJSONResponse{Error: api.ErrorDetail{
		Code:    httpx.CodeBodyTooLarge,
		Message: fmt.Sprintf("the file is larger than this server allows (%d bytes)", limit),
	}}
}

func unsupported() api.ErrorJSONResponse {
	return errorBody(httpx.CodeInvalidRequest, "this kind of file is not accepted")
}

func badRequest(message string) api.ErrorJSONResponse {
	return errorBody(httpx.CodeInvalidRequest, message)
}

func internal(message string) api.ErrorJSONResponse {
	return errorBody(httpx.CodeInternal, message)
}

func errorBody(code, message string) api.ErrorJSONResponse {
	return api.ErrorJSONResponse{Error: api.ErrorDetail{Code: code, Message: message}}
}
