package slack

import (
	"bytes"
	"encoding/json"
	"io"
	"io/ioutil"
	"net/http"

	"github.com/pkg/errors"
	"github.com/slack-go/slack"
	"github.com/target/goalert/config"
	"github.com/target/goalert/notification"
	"github.com/target/goalert/permission"
	"github.com/target/goalert/util/errutil"
)

// Handler responds to API requests from Slack
type Handler struct {
	c Config
	r notification.Receiver
}

// NewHandler creates a new Handler, registering API routes using chi.
func NewHandler(c Config) *Handler {
	return &Handler{c: c}
}

type buttonType int

const (
	buttonTypeAck buttonType = iota
	buttonTypeEsc
	buttonTypeClose
	buttonTypeOpenLink
)

// validRequest is used to validate a request from Slack.
// If the request is validated true is returned, false otherwise.
// https://api.slack.com/authentication/verifying-requests-from-slack
func validRequest(w http.ResponseWriter, req *http.Request) error {
	if req.Method != "POST" {
		return errors.New("not a post")
	}

	secret := config.FromContext(req.Context()).Slack.SigningSecret
	sv, err := slack.NewSecretsVerifier(req.Header, secret)
	if err != nil {
		return err
	}

	defer req.Body.Close()
	body, err := ioutil.ReadAll(io.TeeReader(req.Body, &sv))
	if err != nil {
		return err
	}
	req.Body = ioutil.NopCloser(bytes.NewBuffer(body))

	return sv.Ensure()
}

// ServeActionCallback processes POST requests from Slack. A callback ID is provided
// to determine which action to take.
func (s *ChannelSender) ServeActionCallback(w http.ResponseWriter, req *http.Request) {
	err := validRequest(w, req)
	if err != nil {
		errutil.HTTPError(req.Context(), w, err)
	}

	var payload slack.InteractionCallback
	err = json.Unmarshal([]byte(req.FormValue("payload")), &payload)
	if err != nil {
		errutil.HTTPError(req.Context(), w, err)
	}

	ctx := permission.UserSourceContext(req.Context(), payload.User.ID, permission.RoleUser, &permission.SourceInfo{
		Type: permission.SourceTypeNotificationCallback,
		ID:   "slack:" + payload.Team.ID + ":" + payload.User.ID,
	})
	cfg := config.FromContext(ctx)
	var api = slack.New(cfg.Slack.AccessToken)

	ctx = permission.UserSourceContext(ctx, payload.User.ID, permission.RoleUser, &permission.SourceInfo{
		Type: permission.SourceTypeNotificationChannel,
		ID:   payload.Channel.ID,
	})

	// actions may come in batches, range over
	for _, action := range payload.ActionCallback.BlockActions {
		if action.ActionID == "openLink" {
			return
		}

		// process action
		var actionType notification.Result
		switch action.ActionID {
		case "ack":
			actionType = notification.ResultAcknowledge
		case "esc":
			actionType = notification.ResultEscalate
		case "close":
			actionType = notification.ResultResolve
		default:
			errutil.HTTPError(ctx, w, errors.New("unknown action"))
			return
		}

		a, err := s.r.ReceiveFor(ctx, action.Value, "slack:"+payload.Team.ID, payload.User.ID, actionType)
		if err != nil {
			// Send Unauthorized message back to user in slack if no user is found
			if err.Error() == "user not found for subject and provider ID" {
				_, err := api.PostEphemeralContext(ctx, payload.Channel.ID, payload.User.ID, needsAuthMsgOpt())
				if err != nil {
					errutil.HTTPError(ctx, w, err)
				}
				return
			}

			errutil.HTTPError(ctx, w, err)
			return
		}

		// update original message in Slack
		msgOpt := makeAlertMessageOptions(*a, action.Value, cfg.CallbackURL("/alerts/"+action.Value), payload.ResponseURL)
		_, _, err = api.PostMessageContext(ctx, payload.Channel.ID, msgOpt...)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		}
	}
}
