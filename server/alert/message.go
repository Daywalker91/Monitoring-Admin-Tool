package alert

import (
	"fmt"
	"strings"
	"time"
)

// Label liefert die deutsche Bezeichnung eines Regeltyps.
func Label(kind string) string {
	switch kind {
	case KindOffline:
		return "Offline"
	case KindCPU:
		return "CPU"
	case KindMem:
		return "RAM"
	case KindSwap:
		return "Swap"
	case KindDisk:
		return "Disk"
	}
	return kind
}

// subject benennt den betroffenen Messwert, z. B. "Disk /var".
func (e Event) subject() string {
	if e.Alert.Target != "" {
		return Label(e.Alert.Kind) + " " + e.Alert.Target
	}
	return Label(e.Alert.Kind)
}

// Firing meldet, ob das Ereignis ein ausgelöster Alarm ist.
func (e Event) Firing() bool { return e.Type == Firing }

// Title ist die Kurzfassung für Push-Nachricht und Betreff.
func (e Event) Title() string {
	if e.Type == Reminder {
		return "Erinnerung: " + Event{Type: Firing, Alert: e.Alert, Rule: e.Rule, At: e.At}.Title()
	}
	host := e.Alert.Hostname
	if e.Alert.Kind == KindOffline {
		if e.Firing() {
			return host + " ist offline"
		}
		return host + " ist wieder online"
	}
	if e.Firing() {
		return fmt.Sprintf("%s: %s %s", host, e.subject(), pct(e.Alert.Value))
	}
	return fmt.Sprintf("%s: %s wieder normal", host, e.subject())
}

// Body ist der ausführliche Text.
func (e Event) Body() string {
	a := e.Alert
	var b strings.Builder
	started := time.Unix(a.StartedAt, 0)
	if e.Type == Reminder {
		fmt.Fprintf(&b, "Alarm besteht seit %s.\n", duration(e.At.Sub(started)))
	}
	switch {
	case a.Kind == KindOffline && e.Type != Resolved:
		fmt.Fprintf(&b, "Keine Verbindung zum Agenten seit %s.\n", duration(time.Duration(a.Value*float64(time.Minute))))
	case a.Kind == KindOffline:
		// Value ist die Zeit ohne Verbindung beim Auslösen; dazu kommt die Dauer des Alarms.
		offline := time.Duration(a.Value*float64(time.Minute)) + e.At.Sub(started)
		fmt.Fprintf(&b, "Agent wieder verbunden, war %s ohne Verbindung.\n", duration(offline))
	case e.Type != Resolved:
		fmt.Fprintf(&b, "%s über %s", e.subject(), pct(a.Threshold))
		if e.Rule.ForSec > 0 {
			fmt.Fprintf(&b, " seit mindestens %s", duration(time.Duration(e.Rule.ForSec)*time.Second))
		}
		fmt.Fprintf(&b, ", aktuell %s.\n", pct(a.Value))
	default:
		fmt.Fprintf(&b, "%s wieder unter %s. Alarm dauerte %s, Spitzenwert %s.\n",
			e.subject(), pct(a.Threshold-Hysteresis), duration(e.At.Sub(started)), pct(a.Peak))
	}
	fmt.Fprintf(&b, "Host: %s (%s)\nBeginn: %s", a.Hostname, a.HostID, started.Format("02.01.2006 15:04:05"))
	return b.String()
}

func pct(v float64) string {
	return strings.Replace(fmt.Sprintf("%.1f %%", v), ".", ",", 1)
}

// duration formatiert eine Dauer grob: "45 s", "12 min", "3 h 5 min", "2 d 4 h".
func duration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%d d %d h", int(d.Hours())/24, int(d.Hours())%24)
}
