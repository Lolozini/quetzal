// Package mailtmpl renders the mails the panel sends to people — a password
// reset, an invitation, the settings test — in Quetzal's colours (docs/brand):
// Crème by default, Nuit for a reader in dark mode. Each comes with a text
// version, which is what a client that shows no HTML displays.
//
// The logos are PNG renderings of docs/brand/quetzal-lockup.svg and
// quetzal-lockup-dark.svg at 336×83 (twice the size they are shown at), since
// mail clients do not display SVG. To render them again:
//
//	google-chrome --headless=new --default-background-color=00000000 \
//	  --window-size=336,83 --screenshot=lockup.png page.html
//
// where page.html shows the SVG at 336×83 on a transparent background.
package mailtmpl

import (
	"bytes"
	"embed"
	"fmt"
	htmltemplate "html/template"
	"net/url"
	"strings"
	texttemplate "text/template"
	"time"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/notify"
)

//go:embed layout.html layout.txt lockup.png lockup-dark.png
var files embed.FS

var (
	htmlLayout = htmltemplate.Must(htmltemplate.ParseFS(files, "layout.html"))
	textLayout = texttemplate.Must(texttemplate.ParseFS(files, "layout.txt"))
)

const (
	logoID     = "lockup@quetzal"
	logoDarkID = "lockup-dark@quetzal"
)

// Item is a line of a list: a term and what it means.
type Item struct{ Term, Detail string }

// Action is the mail's one button, and the address it opens.
type Action struct{ Label, URL string }

type content struct {
	Subject   string
	Preheader string // the line clients show next to the subject
	Heading   string
	Intro     []string
	Items     []Item
	Action    *Action
	After     []string
	Footer    []string

	LogoID, LogoDarkID string
}

func render(c content) (notify.Mail, error) {
	c.LogoID, c.LogoDarkID = logoID, logoDarkID
	var h, t bytes.Buffer
	if err := htmlLayout.Execute(&h, c); err != nil {
		return notify.Mail{}, err
	}
	if err := textLayout.Execute(&t, c); err != nil {
		return notify.Mail{}, err
	}
	light, err := files.ReadFile("lockup.png")
	if err != nil {
		return notify.Mail{}, err
	}
	dark, err := files.ReadFile("lockup-dark.png")
	if err != nil {
		return notify.Mail{}, err
	}
	return notify.Mail{
		Subject: c.Subject,
		Text:    t.String(),
		HTML:    h.String(),
		Inline: []notify.Inline{
			{ID: logoID, Name: "quetzal.png", ContentType: "image/png", Data: light},
			{ID: logoDarkID, Name: "quetzal-dark.png", ContentType: "image/png", Data: dark},
		},
	}, nil
}

// panelHost names the panel by the host of its public address, or plainly
// when it has none.
func panelHost(publicURL string) string {
	if u, err := url.Parse(strings.TrimSpace(publicURL)); err == nil && u.Host != "" {
		return u.Host
	}
	return "the Quetzal panel"
}

// sentBy is the footer line that says where a mail comes from.
func sentBy(publicURL string) string {
	if h := panelHost(publicURL); h != "the Quetzal panel" {
		return "Sent by the Quetzal panel at " + h + "."
	}
	return "Sent by a Quetzal panel."
}

// within says how long a link lasts, as a person says it.
func within(d time.Duration) string {
	switch {
	case d == time.Hour:
		return "within the next hour"
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("within %d days", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("within %d hours", d/time.Hour)
	}
	return "within " + d.String()
}

// PasswordReset is the mail sent when someone asks for a new password.
func PasswordReset(username, link string, valid time.Duration, publicURL string) (notify.Mail, error) {
	host := panelHost(publicURL)
	return render(content{
		Subject:   "Reset your Quetzal password",
		Preheader: "A link to choose a new password, " + within(valid) + ".",
		Heading:   "Reset your password",
		Intro: []string{
			fmt.Sprintf("Hi %s, someone asked to reset the password of your account on %s. If it was you, choose a new one:", username, host),
		},
		Action: &Action{Label: "Choose a new password", URL: link},
		After: []string{
			"The link works once, " + within(valid) + ". Choosing a new password signs you out everywhere.",
			"If you did not ask for this, ignore this email: your password stays as it is.",
		},
		Footer: []string{sentBy(publicURL), fmt.Sprintf("It went to the address of the account %s.", username)},
	})
}

// EmailConfirmation is the mail that confirms an account's address.
func EmailConfirmation(username, address, link string, valid time.Duration, publicURL string) (notify.Mail, error) {
	host := panelHost(publicURL)
	return render(content{
		Subject:   "Confirm your email address on Quetzal",
		Preheader: "A link to confirm " + address + ", " + within(valid) + ".",
		Heading:   "Confirm your email address",
		Intro: []string{
			fmt.Sprintf("Hi %s, the account %s on %s gave this address, %s, as its own. If it was you, confirm it:", username, username, host, address),
		},
		Action: &Action{Label: "Confirm this address", URL: link},
		After: []string{
			"The link works once, " + within(valid) + ". Until then the account keeps the address it had, and password resets go there.",
			"If you did not ask for this, ignore this email: the address stays out of the account.",
		},
		Footer: []string{sentBy(publicURL)},
	})
}

// permissionHelp is what each permission allows, worded as in the panel.
var permissionHelp = map[string]string{
	models.PermView:      "its page: state, address, usage, backups, schedules and activity",
	models.PermPower:     "start, stop, restart and kill it",
	models.PermConsole:   "its live console and setup log, commands included",
	models.PermSchedules: "its scheduled tasks, within the other permissions",
	models.PermBackups:   "take, restore and delete backups",
	models.PermFiles:     "its files, from the panel and over SFTP",
	models.PermSettings:  "its variables, resources, ports, exposure, hibernation, reinstall",
	models.PermDatabases: "its databases and their passwords",
	models.PermDelete:    "delete it",
}

// Invitation is the mail that invites someone to a server.
func Invitation(from, server string, perms []string, link string, expires time.Time, signup bool, publicURL string) (notify.Mail, error) {
	host := panelHost(publicURL)
	items := make([]Item, 0, len(perms))
	for _, p := range perms {
		term := p
		if term != "" {
			term = strings.ToUpper(term[:1]) + term[1:]
		}
		items = append(items, Item{Term: term, Detail: permissionHelp[p]})
	}
	how := "Accept it from your account on this panel, or create one from the link."
	if !signup {
		how = "Accept it from your account on this panel."
	}
	until := expires.UTC().Format("2 January 2006, 15:04 UTC")
	return render(content{
		Subject:   fmt.Sprintf("Invitation to %s on Quetzal", server),
		Preheader: fmt.Sprintf("%s invited you to the game server %s.", from, server),
		Heading:   fmt.Sprintf("You are invited to %s", server),
		Intro: []string{
			fmt.Sprintf("%s invited you to the game server %s on %s, with access to:", from, server, host),
		},
		Items:  items,
		Action: &Action{Label: "See the invitation", URL: link},
		After: []string{
			how + " The link works once, until " + until + ".",
			"If you were not expecting it, ignore this email: nothing happens unless you accept.",
		},
		Footer: []string{sentBy(publicURL), fmt.Sprintf("It came to this address because %s entered it.", from)},
	})
}

// Test is the mail an administrator sends to check the email settings. It
// looks like the others, so that it also shows how they will read.
func Test(publicURL string) (notify.Mail, error) {
	return render(content{
		Subject:   "Quetzal test email",
		Preheader: "Your email settings work.",
		Heading:   "Email works",
		Intro: []string{
			fmt.Sprintf("This is a test from %s. Your email settings work: password resets and invitations will look like this.", panelHost(publicURL)),
		},
		Footer: []string{sentBy(publicURL), "An administrator asked for it in the email settings."},
	})
}
