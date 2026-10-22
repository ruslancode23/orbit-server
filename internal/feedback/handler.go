package feedback

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
	"github.com/ruslancode23/orbit-server/internal/ratelimit"
)

// maxTextPart caps one text field of the form before the service counts its
// characters: 4000 characters of up to four bytes each, and a little more.
const maxTextPart = 17 << 10

type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

func (h *Handler) CreateBugReport(ctx context.Context, req api.CreateBugReportRequestObject) (api.CreateBugReportResponseObject, error) {
	session, ok := auth.SessionFrom(ctx)
	if !ok {
		return api.CreateBugReport401JSONResponse{ErrorJSONResponse: errorBody(httpx.CodeUnauthorized, "sign in first")}, nil
	}
	userID := session.User.ID
	if err := h.svc.CheckLimit(userID); err != nil {
		return h.refusal(ctx, err), nil
	}
	if req.Body == nil {
		return api.CreateBugReport400JSONResponse(api.Error(errorBody(httpx.CodeInvalidRequest, "description: describe what went wrong"))), nil
	}

	draft, screenshot, err := h.readForm(req)
	if err != nil {
		return h.refusal(ctx, err), nil
	}

	report, err := h.svc.Submit(ctx, userID, draft, screenshot)
	if err != nil {
		return h.refusal(ctx, err), nil
	}
	// Only what lets the team find the row: the text stays in the database.
	h.log.InfoContext(ctx, "bug report received",
		slog.String("report_id", report.ID.String()),
		slog.String("user_id", userID.String()),
		slog.Bool("screenshot", report.Screenshot != nil))
	return api.CreateBugReport201JSONResponse{Id: report.ID, CreatedAt: report.CreatedAt}, nil
}

// readForm walks the multipart body once: parts come in the order the client
// wrote them, so the screenshot is read into memory as it passes, up to the limit.
func (h *Handler) readForm(req api.CreateBugReportRequestObject) (Draft, []byte, error) {
	var draft Draft
	var screenshot []byte
	for {
		part, err := req.Body.NextPart()
		if errors.Is(err, io.EOF) {
			return draft, screenshot, nil
		}
		if err != nil {
			return Draft{}, nil, &FieldError{Field: "description", Reason: "the form cannot be read"}
		}
		name := part.FormName()
		if name == "screenshot" {
			screenshot, err = h.svc.ReadScreenshot(part)
			_ = part.Close()
			if err != nil {
				return Draft{}, nil, err
			}
			continue
		}
		target := draft.field(name)
		if target == nil {
			_ = part.Close()
			return Draft{}, nil, &FieldError{Field: name, Reason: "is not a field of a bug report"}
		}
		value, err := io.ReadAll(io.LimitReader(part, maxTextPart+1))
		_ = part.Close()
		if err != nil {
			return Draft{}, nil, fmt.Errorf("read %s: %w", name, err)
		}
		if len(value) > maxTextPart {
			return Draft{}, nil, &FieldError{Field: name, Reason: "is too long"}
		}
		*target = string(value)
	}
}

// field is where a text part of the form goes; nil for a name the form does not have.
func (d *Draft) field(name string) *string {
	switch name {
	case "description":
		return &d.Description
	case "steps":
		return &d.Steps
	case "appVersion":
		return &d.AppVersion
	case "userAgent":
		return &d.UserAgent
	case "route":
		return &d.Route
	}
	return nil
}

func (h *Handler) refusal(ctx context.Context, err error) api.CreateBugReportResponseObject {
	var field *FieldError
	var limited *LimitedError
	switch {
	case errors.As(err, &field):
		return api.CreateBugReport400JSONResponse(api.Error(errorBody(httpx.CodeInvalidRequest, field.Error())))
	case errors.As(err, &limited):
		seconds := ratelimit.Seconds(limited.RetryAfter)
		return api.CreateBugReport429JSONResponse{TooManyRequestsJSONResponse: api.TooManyRequestsJSONResponse{
			Body:    api.Error(errorBody(httpx.CodeTooManyRequests, "too many bug reports, try again later")),
			Headers: api.TooManyRequestsResponseHeaders{RetryAfter: &seconds},
		}}
	case errors.Is(err, ErrScreenshotTooLarge):
		return api.CreateBugReport413JSONResponse(api.Error(errorBody(httpx.CodeBodyTooLarge,
			fmt.Sprintf("screenshot: the image is larger than this server allows (%d bytes)", h.svc.MaxScreenshotBytes()))))
	case errors.Is(err, ErrScreenshotType):
		return api.CreateBugReport415JSONResponse(api.Error(errorBody(httpx.CodeInvalidRequest, "screenshot: must be a PNG, JPEG or WebP image")))
	default:
		h.log.ErrorContext(ctx, "create bug report", slog.Any("error", err))
		return api.CreateBugReport500JSONResponse(api.Error(errorBody(httpx.CodeInternal, "could not store the report")))
	}
}

func errorBody(code, message string) api.ErrorJSONResponse {
	return api.ErrorJSONResponse{Error: api.ErrorDetail{Code: code, Message: message}}
}
