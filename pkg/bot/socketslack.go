package bot

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/kubeshop/botkube/internal/analytics"
	"github.com/kubeshop/botkube/pkg/bot/interactive"
	"github.com/kubeshop/botkube/pkg/config"
	"github.com/kubeshop/botkube/pkg/events"
	"github.com/kubeshop/botkube/pkg/execute"
	"github.com/kubeshop/botkube/pkg/execute/command"
	"github.com/kubeshop/botkube/pkg/execute/kubectl"
	"github.com/kubeshop/botkube/pkg/multierror"
	"github.com/kubeshop/botkube/pkg/sliceutil"
	"github.com/kubeshop/botkube/pkg/utils"
)

// TODO: Refactor this file as a part of https://github.com/kubeshop/botkube/issues/667
//    - handle and send methods from `slackMessage` should be defined on Bot level,
//    - split to multiple files in a separate package,
//    - review all the methods and see if they can be simplified.

var _ Bot = &SocketSlack{}

// EventCommandProvider describes a provider for event commands.
type EventCommandProvider interface {
	GetCommandsForEvent(event events.Event, executorBindings []string) ([]kubectl.Command, error)
}

// SocketSlack listens for user's message, execute commands and sends back the response.
type SocketSlack struct {
	log              logrus.FieldLogger
	executorFactory  ExecutorFactory
	reporter         socketSlackAnalyticsReporter
	eventCmdProvider EventCommandProvider
	botID            string
	client           *slack.Client
	channelsMutex    sync.RWMutex
	channels         map[string]channelConfigByName
	notifyMutex      sync.Mutex
	botMentionRegex  *regexp.Regexp
	commGroupName    string
	renderer         *SlackRenderer
	mdFormatter      interactive.MDFormatter
}

type socketSlackMessage struct {
	Text            string
	Channel         string
	ThreadTimeStamp string
	User            string
	TriggerID       string
	CommandOrigin   command.Origin
	State           *slack.BlockActionStates
	ResponseURL     string
	BlockID         string
}

// socketSlackAnalyticsReporter defines a reporter that collects analytics data.
type socketSlackAnalyticsReporter interface {
	FatalErrorAnalyticsReporter
	ReportCommand(platform config.CommPlatformIntegration, command string, origin command.Origin, withFilter bool) error
}

// NewSocketSlack creates a new SocketSlack instance.
func NewSocketSlack(log logrus.FieldLogger, commGroupName string, cfg config.SocketSlack, executorFactory ExecutorFactory, eventCmdProvider EventCommandProvider, reporter socketSlackAnalyticsReporter) (*SocketSlack, error) {
	client := slack.New(cfg.BotToken, slack.OptionAppLevelToken(cfg.AppToken))

	authResp, err := client.AuthTest()
	if err != nil {
		return nil, fmt.Errorf("while testing the ability to do auth Slack request: %w", err)
	}
	botID := authResp.UserID

	botMentionRegex, err := slackBotMentionRegex(botID)
	if err != nil {
		return nil, err
	}

	channels := slackChannelsConfigFrom(cfg.Channels)
	if err != nil {
		return nil, fmt.Errorf("while producing channels configuration map by ID: %w", err)
	}

	mdFormatter := interactive.NewMDFormatter(interactive.NewlineFormatter, mdHeaderFormatter)
	return &SocketSlack{
		log:              log,
		executorFactory:  executorFactory,
		reporter:         reporter,
		botID:            botID,
		client:           client,
		channels:         channels,
		commGroupName:    commGroupName,
		eventCmdProvider: eventCmdProvider,
		renderer:         NewSlackRenderer(cfg.Notification),
		botMentionRegex:  botMentionRegex,
		mdFormatter:      mdFormatter,
	}, nil
}

// Start starts the Slack WebSocket connection and listens for messages
func (b *SocketSlack) Start(ctx context.Context) error {
	b.log.Info("Starting bot")

	websocketClient := socketmode.New(b.client)

	go func() {
		defer analytics.ReportPanicIfOccurs(b.log, b.reporter)
		socketRunErr := websocketClient.RunContext(ctx)
		if socketRunErr != nil {
			reportErr := b.reporter.ReportFatalError(socketRunErr)
			if reportErr != nil {
				b.log.Errorf("while reporting socket error: %s", reportErr.Error())
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			b.log.Info("Shutdown requested. Finishing...")
			return nil
		case event := <-websocketClient.Events:
			switch event.Type {
			case socketmode.EventTypeConnecting:
				b.log.Info("Botkube is connecting to Slack...")
			case socketmode.EventTypeConnected:
				if err := b.reporter.ReportBotEnabled(b.IntegrationName()); err != nil {
					return fmt.Errorf("report analytics error: %w", err)
				}
				b.log.Info("Botkube connected to Slack!")
			case socketmode.EventTypeEventsAPI:
				eventsAPIEvent, ok := event.Data.(slackevents.EventsAPIEvent)
				if !ok {
					b.log.Errorf("Invalid event %+v\n", event.Data)
					continue
				}
				websocketClient.Ack(*event.Request)
				if eventsAPIEvent.Type == slackevents.CallbackEvent {
					b.log.Debugf("Got callback event %s", utils.StructDumper().Sdump(eventsAPIEvent))
					innerEvent := eventsAPIEvent.InnerEvent
					switch ev := innerEvent.Data.(type) {
					case *slackevents.AppMentionEvent:
						b.log.Debugf("Got app mention %s", utils.StructDumper().Sdump(innerEvent))
						msg := socketSlackMessage{
							Text:            ev.Text,
							Channel:         ev.Channel,
							ThreadTimeStamp: ev.ThreadTimeStamp,
							User:            ev.User,
							CommandOrigin:   command.TypedOrigin,
						}
						if err := b.handleMessage(ctx, msg); err != nil {
							b.log.Errorf("Message handling error: %s", err.Error())
						}
					}
				}
			case socketmode.EventTypeInteractive:
				callback, ok := event.Data.(slack.InteractionCallback)
				if !ok {
					b.log.Errorf("Invalid event %+v\n", event.Data)
					continue
				}

				websocketClient.Ack(*event.Request)

				switch callback.Type {
				case slack.InteractionTypeBlockActions:
					b.log.Debugf("Got block action %s", utils.StructDumper().Sdump(callback.ActionCallback.BlockActions))

					if len(callback.ActionCallback.BlockActions) != 1 {
						b.log.Debug("Ignoring callback as the number of actions is different from 1")
						continue
					}

					act := callback.ActionCallback.BlockActions[0]
					if act == nil || strings.HasPrefix(act.ActionID, urlButtonActionIDPrefix) {
						reportErr := b.reporter.ReportCommand(b.IntegrationName(), act.ActionID, command.ButtonClickOrigin, false)
						if reportErr != nil {
							b.log.Errorf("while reporting URL command, error: %s", reportErr.Error())
						}
						continue // skip the url actions
					}

					channelID := callback.Channel.ID
					if channelID == "" && callback.View.ID != "" {
						// TODO: add support when we will need to handle button clicks from active modal.
						//
						// The request is coming from active modal, currently we don't support that.
						// We process that only when the modal is submitted (see slack.InteractionTypeViewSubmission action type).
						b.log.Debug("Ignoring callback as its source is an active modal")
						continue
					}

					cmd, cmdOrigin := resolveBlockActionCommand(*act)
					// Use thread's TS if interactive call triggered within thread.
					threadTs := callback.MessageTs
					if callback.Message.Msg.ThreadTimestamp != "" {
						threadTs = callback.Message.Msg.ThreadTimestamp
					}
					msg := socketSlackMessage{
						Text:            cmd,
						Channel:         channelID,
						ThreadTimeStamp: threadTs,
						TriggerID:       callback.TriggerID,
						User:            callback.User.ID,
						CommandOrigin:   cmdOrigin,
						State:           callback.BlockActionState,
						ResponseURL:     callback.ResponseURL,
						BlockID:         act.BlockID,
					}
					if err := b.handleMessage(ctx, msg); err != nil {
						b.log.Errorf("Message handling error: %s", err.Error())
					}
				case slack.InteractionTypeViewSubmission: // this event is received when modal is submitted

					// the map key is the ID of the input block, for us, it's autogenerated
					for _, item := range callback.View.State.Values {
						for actID, act := range item {
							act.ActionID = actID // normalize event

							cmd, cmdOrigin := resolveBlockActionCommand(act)
							msg := socketSlackMessage{
								Text:          cmd,
								Channel:       callback.View.PrivateMetadata,
								User:          callback.User.ID,
								CommandOrigin: cmdOrigin,
							}

							if err := b.handleMessage(ctx, msg); err != nil {
								b.log.Errorf("Message handling error: %s", err.Error())
							}
						}
					}
				default:
					b.log.Debugf("get unhandled event %s", callback.Type)
				}

			case socketmode.EventTypeErrorBadMessage:
				b.log.Errorf("Bad message: %+v\n", event.Data)
			case socketmode.EventTypeIncomingError:
				b.log.Errorf("Incoming error: %+v\n", event.Data)
			case socketmode.EventTypeConnectionError:
				b.log.Errorf("Slack connection error: %+v\n", event.Data)
			}
		}
	}
}

// Type describes the notifier type.
func (b *SocketSlack) Type() config.IntegrationType {
	return config.BotIntegrationType
}

// IntegrationName describes the notifier integration name.
func (b *SocketSlack) IntegrationName() config.CommPlatformIntegration {
	return config.SocketSlackCommPlatformIntegration
}

// NotificationsEnabled returns current notification status for a given channel name.
func (b *SocketSlack) NotificationsEnabled(channelName string) bool {
	channel, exists := b.getChannels()[channelName]
	if !exists {
		return false
	}

	return channel.notify
}

// SetNotificationsEnabled sets a new notification status for a given channel name.
func (b *SocketSlack) SetNotificationsEnabled(channelName string, enabled bool) error {
	// avoid race conditions with using the setter concurrently, as we set whole map
	b.notifyMutex.Lock()
	defer b.notifyMutex.Unlock()

	channels := b.getChannels()
	channel, exists := channels[channelName]
	if !exists {
		return execute.ErrNotificationsNotConfigured
	}

	channel.notify = enabled
	channels[channelName] = channel
	b.setChannels(channels)

	return nil
}

func (b *SocketSlack) handleMessage(ctx context.Context, event socketSlackMessage) error {
	// Handle message only if starts with mention
	request, found := b.findAndTrimBotMention(event.Text)
	if !found {
		b.log.Debugf("Ignoring message as it doesn't contain %q mention", b.botID)
		return nil
	}

	b.log.Debugf("Slack incoming Request: %s", request)

	// Unfortunately we need to do a call for channel name based on ID every time a message arrives.
	// I wanted to query for channel IDs based on names and prepare a map in the `slackChannelsConfigFrom`,
	// but unfortunately Botkube would need another scope (get all conversations).
	// Keeping current way of doing this until we come up with a better idea.
	info, err := b.client.GetConversationInfo(event.Channel, true)
	if err != nil {
		return fmt.Errorf("while getting conversation info: %w", err)
	}

	channel, isAuthChannel := b.getChannels()[info.Name]

	e := b.executorFactory.NewDefault(execute.NewDefaultInput{
		CommGroupName:   b.commGroupName,
		Platform:        b.IntegrationName(),
		NotifierHandler: b,
		Conversation: execute.Conversation{
			Alias:            channel.alias,
			ID:               channel.Identifier(),
			ExecutorBindings: channel.Bindings.Executors,
			IsAuthenticated:  isAuthChannel,
			CommandOrigin:    event.CommandOrigin,
			State:            event.State,
		},
		Message: request,
		User:    fmt.Sprintf("<@%s>", event.User),
	})
	response := e.Execute(ctx)
	err = b.send(event, response)
	if err != nil {
		return fmt.Errorf("while sending message: %w", err)
	}

	return nil
}

func (b *SocketSlack) send(event socketSlackMessage, resp interactive.Message) error {
	b.log.Debugf("Slack Response: %s", resp)

	markdown := interactive.RenderMessage(b.mdFormatter, resp)

	if len(markdown) == 0 {
		return errors.New("while reading Slack response: empty response")
	}

	// Upload message as a file if too long
	var file *slack.File
	var err error
	if len(markdown) >= slackMaxMessageSize {
		file, err = uploadFileToSlack(event.Channel, resp, b.client, event.ThreadTimeStamp)
		if err != nil {
			return err
		}
		resp = interactive.Message{
			PlaintextInputs: resp.PlaintextInputs,
		}
	}

	// we can open modal only if we have a TriggerID (it's available when user clicks a button)
	if resp.Type == interactive.Popup && event.TriggerID != "" {
		modalView := b.renderer.RenderModal(resp)
		modalView.PrivateMetadata = event.Channel
		_, err := b.client.OpenView(event.TriggerID, modalView)
		if err != nil {
			return fmt.Errorf("while opening modal: %w", err)
		}
		return nil
	}

	options := []slack.MsgOption{
		b.renderer.RenderInteractiveMessage(resp),
	}

	if ts := b.getThreadOptionIfNeeded(event, file); ts != nil {
		options = append(options, ts)
	}

	if resp.ReplaceOriginal && event.ResponseURL != "" {
		options = append(options, slack.MsgOptionReplaceOriginal(event.ResponseURL))
	}

	if resp.OnlyVisibleForYou {
		if _, err := b.client.PostEphemeral(event.Channel, event.User, options...); err != nil {
			return fmt.Errorf("while posting Slack message visible only to user: %w", err)
		}
	} else {
		if _, _, err := b.client.PostMessage(event.Channel, options...); err != nil {
			return fmt.Errorf("while posting Slack message: %w", err)
		}
	}

	return nil
}

// SendEvent sends event notification to slack
func (b *SocketSlack) SendEvent(ctx context.Context, event events.Event, eventSources []string) error {
	b.log.Debugf("Sending to Slack: %+v", event)

	errs := multierror.New()
	for _, channelName := range b.getChannelsToNotifyForEvent(event, eventSources) {
		additionalSection := b.getInteractiveEventSectionIfShould(event, channelName)

		var additionalSections []interactive.Section
		if additionalSection != nil {
			additionalSections = append(additionalSections, *additionalSection)
		}
		msg := b.renderer.RenderEventMessage(event, additionalSections...)

		options := []slack.MsgOption{
			b.renderer.RenderInteractiveMessage(msg),
		}

		channelID, timestamp, err := b.client.PostMessageContext(ctx, channelName, options...)
		if err != nil {
			errs = multierror.Append(errs, fmt.Errorf("while posting message to channel %q: %w", channelName, err))
			continue
		}

		b.log.Debugf("Event successfully sent to channel %q (ID: %q) at %b", channelName, channelID, timestamp)
	}

	return errs.ErrorOrNil()
}

func (b *SocketSlack) getInteractiveEventSectionIfShould(event events.Event, channelName string) *interactive.Section {
	channel, isAuthChannel := b.getChannels()[channelName]
	if !isAuthChannel {
		return nil
	}

	commands, err := b.eventCmdProvider.GetCommandsForEvent(event, channel.Bindings.Executors)
	if err != nil {
		b.log.Errorf("while getting commands for event: %w", err)
		return nil
	}

	if len(commands) == 0 {
		return nil
	}

	cmdPrefix := fmt.Sprintf("%s kubectl", b.BotName())
	var optionItems []interactive.OptionItem
	for _, cmd := range commands {
		optionItems = append(optionItems, interactive.OptionItem{
			Name:  cmd.Name,
			Value: cmd.Cmd,
		})
	}
	section := interactive.EventCommandsSection(cmdPrefix, optionItems)
	return &section
}

func (b *SocketSlack) getChannelsToNotifyForEvent(event events.Event, sourceBindings []string) []string {
	// support custom event routing
	if event.Channel != "" {
		return []string{event.Channel}
	}

	return b.getChannelsToNotify(sourceBindings)
}

func (b *SocketSlack) getChannelsToNotify(sourceBindings []string) []string {
	var out []string
	for _, cfg := range b.getChannels() {
		if !cfg.notify {
			b.log.Infof("Skipping notification for channel %q as notifications are disabled.", cfg.Identifier())
			continue
		}

		if !sliceutil.Intersect(sourceBindings, cfg.Bindings.Sources) {
			continue
		}

		out = append(out, cfg.Identifier())
	}
	return out
}

// SendGenericMessage sends message with interactive sections to selectred Slack channels.
func (b *SocketSlack) SendGenericMessage(_ context.Context, genericMsg interactive.GenericMessage, sourceBindings []string) error {
	msg := genericMsg.ForBot(b.BotName())

	errs := multierror.New()
	for _, channelName := range b.getChannelsToNotify(sourceBindings) {
		b.log.Debugf("Sending message to channel %q: %+v", channelName, msg)

		msgMetadata := socketSlackMessage{
			Channel:         channelName,
			ThreadTimeStamp: "",
			BlockID:         uuid.New().String(),
			CommandOrigin:   command.AutomationOrigin,
		}
		err := b.send(msgMetadata, msg)
		if err != nil {
			errs = multierror.Append(errs, fmt.Errorf("while sending Slack message to channel %q: %w", channelName, err))
			continue
		}
		b.log.Debugf("Message successfully sent to channel %q", channelName)
	}

	return errs.ErrorOrNil()
}

// SendMessageToAll sends message with interactive sections to all Slack channels.
func (b *SocketSlack) SendMessageToAll(ctx context.Context, msg interactive.Message) error {
	errs := multierror.New()
	for _, channel := range b.getChannels() {
		channelName := channel.Name
		b.log.Debugf("Sending message to channel %q (alias: %q): %+v", channelName, channel.alias, msg)

		message := b.renderer.RenderInteractiveMessage(msg)

		channelID, timestamp, err := b.client.PostMessageContext(ctx, channelName, message)
		if err != nil {
			errs = multierror.Append(errs, fmt.Errorf("while sending Slack message to channel %q (alias: %q): %w", channelName, channel.alias, err))
			continue
		}
		b.log.Debugf("Message successfully sent to channel %q (alias: %q) at %q", channelID, channel.alias, timestamp)
	}

	return errs.ErrorOrNil()
}

// BotName returns the Bot name.
func (b *SocketSlack) BotName() string {
	return fmt.Sprintf("<@%s>", b.botID)
}

func (b *SocketSlack) getChannels() map[string]channelConfigByName {
	b.channelsMutex.RLock()
	defer b.channelsMutex.RUnlock()
	return b.channels
}

func (b *SocketSlack) setChannels(channels map[string]channelConfigByName) {
	b.channelsMutex.Lock()
	defer b.channelsMutex.Unlock()
	b.channels = channels
}

func (b *SocketSlack) findAndTrimBotMention(msg string) (string, bool) {
	if !b.botMentionRegex.MatchString(msg) {
		return "", false
	}

	return b.botMentionRegex.ReplaceAllString(msg, ""), true
}

func resolveBlockActionCommand(act slack.BlockAction) (string, command.Origin) {
	cmd := act.Value
	cmdOrigin := command.UnknownOrigin

	switch act.Type {
	// currently we support only interactive.MultiSelect option
	case "multi_static_select":
		var items []string
		for _, item := range act.SelectedOptions {
			items = append(items, item.Value)
		}
		cmd = fmt.Sprintf("%s %s", act.ActionID, strings.Join(items, ","))
		cmdOrigin = command.MultiSelectValueChangeOrigin
	case "static_select":
		// Example of commands that are handled here:
		//   @Botkube kcc --verbs get
		//   @Botkube kcc --resource-type
		cmd = fmt.Sprintf("%s %s", act.ActionID, act.SelectedOption.Value)
		cmdOrigin = command.SelectValueChangeOrigin
	case "button":
		cmdOrigin = command.ButtonClickOrigin
	case "plain_text_input":
		cmd = fmt.Sprintf("%s%q", act.BlockID, strings.TrimSpace(act.Value))
		cmdOrigin = command.PlainTextInputOrigin
	}

	return cmd, cmdOrigin
}

func (b *SocketSlack) getThreadOptionIfNeeded(event socketSlackMessage, file *slack.File) slack.MsgOption {
	//if the message is from thread then add an option to return the response to the thread
	if event.ThreadTimeStamp != "" {
		return slack.MsgOptionTS(event.ThreadTimeStamp)
	}

	if file == nil {
		return nil
	}

	// If the message was already as a file attachment, reply it a given thread
	for _, share := range file.Shares.Public {
		if len(share) >= 1 && share[0].Ts != "" {
			return slack.MsgOptionTS(share[0].Ts)
		}
	}

	return nil
}
