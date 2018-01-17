package main

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"

	"github.com/burntsushi/toml"
	"github.com/mattermost/mattermost-server/model"
	"github.com/yhat/scrape"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const (
	VERSION = "v0.1"

	CANTEEN_URL_TODAY    = "http://speiseplan.studierendenwerk-hamburg.de/de/580/2018/0/"
	CANTEEN_URL_TOMORROW = "http://speiseplan.studierendenwerk-hamburg.de/de/580/2018/99/"
)

type config struct {
	MattermostApiURL string
	MattermostWsURL  string
	UserEmail        string
	UserPassword     string
	TeamName         string

	DisplayName string
	MentionName string

	ChannelNameDebug      string
	ChannelNameProduction string

	Favorites []string
}

var CONFIG config

type dish struct {
	name            string
	prices          [3]string
	isVegetarian    bool
	isVegan         bool
	containsBeef    bool
	containsPork    bool
	containsFish    bool
	containsChicken bool
	lactoseFree     bool
}

type mensabot struct {
	client   *model.Client4
	wsClient *model.WebSocketClient

	user *model.User
	team *model.Team

	channelDebug      *model.Channel
	channelProduction *model.Channel
}

func (d dish) isFavorite() bool {
	name := strings.ToLower(d.name)
	for _, f := range CONFIG.Favorites {
		if strings.Contains(name, f) {
			return true
		}
	}
	return false
}

func (d dish) String() string {
	var buf bytes.Buffer
	buf.WriteString("| " + d.name + " |")
	if d.isFavorite() {
		buf.WriteString(" :heart_eyes:")
	}
	if d.isVegan {
		buf.WriteString(" :sunflower:")
	} else if d.isVegetarian {
		buf.WriteString(" :carrot:")
	}
	if d.containsBeef {
		buf.WriteString(" :cow2:")
	}
	if d.containsPork {
		buf.WriteString(" :pig2:")
	}
	if d.containsFish {
		buf.WriteString(" :fish:")
	}
	if d.containsChicken {
		buf.WriteString(" :rooster:")
	}
	if d.lactoseFree {
		buf.WriteString(" :milk_glass:")
	}
	buf.WriteString(" |")
	buf.WriteString(fmt.Sprintf(" %s // %s // %s |", d.prices[0], d.prices[1], d.prices[2]))
	return buf.String()
}

func trimNodeName(name string) (trimmed string) {
	trimmed = strings.Trim(name, " \t\n")
	trimmed = strings.Replace(trimmed, "( ", "(", -1)
	trimmed = strings.Replace(trimmed, " )", ")", -1)
	trimmed = strings.Replace(trimmed, " ,", ",", -1)
	trimmed = strings.Replace(trimmed, "  ", " ", -1)

	return
}

func dishFromNode(node *html.Node) dish {
	name := trimNodeName(scrape.Text(node))

	var prices [3]string
	var isVegetarian bool
	var isVegan bool
	var containsBeef bool
	var containsPork bool
	var containsFish bool
	var containsChicken bool
	var lactoseFree bool

	priceNodes := scrape.FindAll(node.Parent, scrape.ByClass("price"))
	imgNodes := scrape.FindAll(node, scrape.ByTag(atom.Img))

	for i, price := range priceNodes {
		prices[i] = strings.Replace(scrape.Text(price), "\xc2\xa0", "", -1)
	}

	for _, img := range imgNodes {
		switch strings.ToLower(scrape.Attr(img, "title")) {
		case "vegetarisch":
			isVegetarian = true
		case "vegan":
			isVegan = true
		case "mit rind":
			containsBeef = true
		case "mit schwein":
			containsPork = true
		case "mit fisch":
			containsFish = true
		case "mit geflügel":
			containsChicken = true
		case "laktosefrei":
			lactoseFree = true
		}
	}

	return dish{name, prices, isVegetarian || isVegan, isVegan, containsBeef, containsPork, containsFish, containsChicken, lactoseFree}
}

func getCanteenPlan(url string) (dishes []dish) {
	resp, err := http.Get(url)
	if err != nil {
		panic(err)
	}
	root, err := html.Parse(resp.Body)
	if err != nil {
		panic(err)
	}

	dishNodes := scrape.FindAll(root, scrape.ByClass("dish-description"))

	for _, dn := range dishNodes {
		dishes = append(dishes, dishFromNode(dn))
	}

	return
}

func newMensaBotFromConfig(cfg *config) (bot *mensabot) {
	println("[newMensaBotFromConfig] Connecting to " + cfg.MattermostApiURL)
	client := model.NewAPIv4Client(cfg.MattermostApiURL)

	bot = &mensabot{client: client}

	bot.setupGracefulShutdown()
	bot.ensureServerIsRunning()
	bot.loginAsBotUser(cfg.UserEmail, cfg.UserPassword)
	bot.setTeam(cfg.TeamName)

	// WebSocket client needs the AuthToken from bot::loginAsBotUser
	if wsClient, err := model.NewWebSocketClient4(cfg.MattermostWsURL, client.AuthToken); err != nil {
		println("[newMensaBotFromConfig] Failed to connect to the web socket")
		printError(err)
		panic(err)
	} else {
		bot.wsClient = wsClient
	}

	bot.channelDebug = bot.getChannel(cfg.ChannelNameDebug)
	bot.channelProduction = bot.getChannel(cfg.ChannelNameProduction)

	return
}

func (bot *mensabot) setupGracefulShutdown() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		for _ = range c {
			if bot.wsClient != nil {
				bot.wsClient.Close()
			}

			bot.sendMessage("_["+CONFIG.DisplayName+"] has **stopped** running_", bot.channelDebug.Id, "")
			os.Exit(0)
		}
	}()
}

func (bot *mensabot) ensureServerIsRunning() {
	if props, resp := bot.client.GetOldClientConfig(""); resp.Error != nil {
		println("There was a problem pinging the Mattermost server.  Are you sure it's running?")
		printError(resp.Error)
		os.Exit(1)
	} else {
		println("[bot::ensureServerIsRunning] Server detected and is running version " + props["Version"])
	}
}

func (bot *mensabot) loginAsBotUser(email string, password string) {
	if user, resp := bot.client.Login(email, password); resp.Error != nil {
		println("There was a problem logging into the Mattermost server.")
		printError(resp.Error)
		panic(resp.Error)
	} else {
		println("[bot::loginAsBotUser] Logged in as user '" + email + "': " + user.Id)
		bot.user = user
	}
}

func (bot *mensabot) setTeam(teamName string) {
	if team, resp := bot.client.GetTeamByName(teamName, ""); resp.Error != nil {
		println("We failed to get the initial load")
		println("or we do not appear to be a member of the team '" + teamName + "'")
		printError(resp.Error)
		panic(resp.Error)
	} else {
		println("[bot::setTeam] Got team with name '" + teamName + "`: " + team.Id)
		bot.team = team
	}
}

func (bot *mensabot) getChannel(channelName string) *model.Channel {
	rChan, resp := bot.client.GetChannelByName(channelName, bot.team.Id, "")
	if resp.Error != nil {
		println("We failed to get the channel: " + channelName)
		printError(resp.Error)
		panic(resp.Error)
	}
	println("[bot::getChannel] Got channel with name '" + channelName + "': " + rChan.Id)
	return rChan
}

func (bot *mensabot) sendMessage(msg string, channelID string, replyToID string) {
	post := &model.Post{}
	post.ChannelId = channelID
	post.Message = msg
	post.RootId = replyToID

	if _, resp := bot.client.CreatePost(post); resp.Error != nil {
		println("We failed to send a message to channel: " + channelID)
		printError(resp.Error)
	}
}

func (bot *mensabot) startListening() {
	bot.sendMessage("_["+CONFIG.DisplayName+"] has **started** running_", bot.channelDebug.Id, "")
	bot.wsClient.Listen()

	for {
		select {
		case event := <-bot.wsClient.EventChannel:
			bot.handleWebSocketEvent(event)
		}
	}
}

func (bot *mensabot) handleWebSocketEvent(event *model.WebSocketEvent) {
	// Skip empty events to avoid noise (especially at shutdown)
	if event == nil {
		return
	}

	fmt.Printf("[bot::handleWebSocketEvent] Handling event: %v\n", event)

	// We only care about new posts
	if event.Event != model.WEBSOCKET_EVENT_POSTED {
		return
	}

	post := model.PostFromJson(strings.NewReader(event.Data["post"].(string)))
	if post != nil {
		// ignore my own posts
		if post.UserId == bot.user.Id {
			return
		}

		if strings.HasPrefix(post.Message, CONFIG.MentionName) {
			bot.handleCommand(post)
		} else if event.Broadcast.ChannelId == bot.channelDebug.Id {
			bot.handleCommand(post)
		}
	}
}

func (bot *mensabot) writeDishes(dishes []dish, prefix string, channelID string, replyToID string) {
	var buf bytes.Buffer

	buf.WriteString(prefix + "\n\n")
	buf.WriteString("| Essen | Features | Preise |\n")
	buf.WriteString("| -- | -- | -- |\n")
	for _, d := range dishes {
		buf.WriteString(d.String() + "\n")
	}

	bot.sendMessage(buf.String(), channelID, replyToID)
}

func (bot *mensabot) writeLegend(channelID string, replyToID string) {
	msg := "**Legende:**\n" +
		":heart_eyes: = Lieblingsgericht\n" +
		":sunflower: = Veganes Gericht\n" +
		":carrot: = Vegetarisches Gericht\n" +
		":cow2: = Enthält Rindfleisch\n" +
		":pig2: = Enthält Schweinefleisch\n" +
		":fish: = Enthält Fisch\n" +
		":rooster: = Enthält Geflügel\n" +
		":milk_glass: = Laktose**freies**(!) Gericht\n"

	bot.sendMessage(msg, channelID, replyToID)
}

func (bot *mensabot) writeCommands(channelID string, replyToID string) {
	bot.sendMessage("TODO: Implement commands..", channelID, replyToID)
}

func (bot *mensabot) handleCommand(post *model.Post) {
	println("Handling post: " + post.Message)

	if matched, _ := regexp.MatchString(`(?:^|\W)((a|A)live|(r|R)unning|(u|U)p)(?:$|\W)`, post.Message); matched {
		// If you see any word matching 'alive'/'running'/'up' then respond with status
		bot.sendMessage("Yes I'm up and running!", post.ChannelId, post.Id)
		return
	} else if matched, _ := regexp.MatchString(`(?:^|\W)((h|H)eute|(t|T)oday)(?:$|\W)`, post.Message); matched {
		// If you see any word matching 'heute' or 'today' post today's canteen plan
		dishes := getCanteenPlan(CANTEEN_URL_TODAY)
		bot.writeDishes(dishes, "**Heute gibt es:**", post.ChannelId, post.Id)
	} else if matched, _ := regexp.MatchString(`(?:^|\W)((m|M)orgen|(t|T)omorrow)(?:$|\W)`, post.Message); matched {
		// If you see any word matching 'morgen' or 'tomorrow' post tomorrow's canteen plan
		dishes := getCanteenPlan(CANTEEN_URL_TOMORROW)
		bot.writeDishes(dishes, "**Morgen gibt es:**", post.ChannelId, post.Id)
	} else if matched, _ := regexp.MatchString(`(?:^|\W)((l|L)egend(|e))(?:$|\W)`, post.Message); matched {
		// If you see any word matching 'lengend' write legend
		bot.writeLegend(post.ChannelId, post.Id)
	} else if matched, _ := regexp.MatchString(`(?:^|\W)((c|C)ommmand|(h|H)elp)(?:$|\W)`, post.Message); matched {
		// If you see any word matching 'command' or 'help' write available commands
		bot.writeCommands(post.ChannelId, post.Id)
	} else {
		// If nothing matched return a generic message
		bot.sendMessage("What does this even mean?!", post.ChannelId, post.Id)
	}
}

func readConfig() {
	if len(os.Args) < 2 {
		println("ERROR: MensaBot expects the configuration file as first argument!")
		os.Exit(1)
	}

	cfgFile := os.Args[1]
	_, err := os.Stat(cfgFile)
	if err != nil {
		println("Config file is missing: " + cfgFile)
		panic(err)
	}
	if _, err := toml.DecodeFile(cfgFile, &CONFIG); err != nil {
		panic(err)
	}
}

func main() {
	readConfig()

	bot := newMensaBotFromConfig(&CONFIG)
	go bot.startListening()

	// Forever block main routine
	// TODO |2018-01-17|: It works without this, investigate what the best practices are
	select {}
}

func printError(err *model.AppError) {
	println("\tError Details:")
	println("\t\t" + err.Message)
	println("\t\t" + err.Id)
	println("\t\t" + err.DetailedError)
}