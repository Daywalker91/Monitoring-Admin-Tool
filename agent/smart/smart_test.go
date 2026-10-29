package smart

import (
	"context"
	"strings"
	"testing"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// Gekürzte Ausgaben von smartctl 7.4 (--json).
const (
	scanJSON = `{"smartctl":{"exit_status":0},"devices":[
		{"name":"/dev/sda","info_name":"/dev/sda [SAT]","type":"sat","protocol":"ATA"},
		{"name":"/dev/sdb","info_name":"/dev/sdb [SAT]","type":"sat","protocol":"ATA"},
		{"name":"/dev/nvme0","info_name":"/dev/nvme0","type":"nvme","protocol":"NVMe"},
		{"name":"/dev/sdc","info_name":"/dev/sdc","type":"sat","protocol":"ATA"},
		{"name":"/dev/sdx;reboot","type":"sat"}]}`
	ssdJSON = `{"smartctl":{"exit_status":0},"model_name":"Samsung SSD 870 EVO 1TB","serial_number":"S6P","firmware_version":"SVT02B6Q",
		"user_capacity":{"bytes":1000204886016},"smart_status":{"passed":true},"temperature":{"current":31},
		"power_on_time":{"hours":8123},"power_cycle_count":412,"ata_smart_attributes":{"table":[
		{"id":5,"name":"Reallocated_Sector_Ct","raw":{"value":0}},{"id":197,"name":"Current_Pending_Sector","raw":{"value":0}},
		{"id":199,"name":"UDMA_CRC_Error_Count","raw":{"value":2}}]}}`
	hddJSON = `{"smartctl":{"exit_status":64},"model_name":"WDC WD40EFRX","smart_status":{"passed":true},"temperature":{"current":38},
		"ata_smart_attributes":{"table":[{"id":5,"raw":{"value":24}},{"id":197,"raw":{"value":8}},{"id":198,"raw":{"value":0}}]}}`
	nvmeJSON = `{"smartctl":{"exit_status":0},"model_name":"WD Blue SN570 1TB","smart_status":{"passed":true},"temperature":{"current":44},
		"power_on_time":{"hours":3021},"nvme_smart_health_information_log":{"critical_warning":0,"available_spare":100,
		"available_spare_threshold":10,"percentage_used":3,"media_errors":0}}`
	standbyJSON = `{"smartctl":{"exit_status":2,"messages":[{"string":"Device is in STANDBY mode, exit(2)","severity":"information"}]}}`
)

// fakeReader liefert je Gerät (letztes Argument) eine feste Ausgabe.
func fakeReader(outputs map[string]string) (*Reader, *[]string) {
	var calls []string
	r := &Reader{path: "smartctl", last: map[string]proto.SmartDisk{}}
	r.run = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return []byte(outputs[args[len(args)-1]]), nil
	}
	return r, &calls
}

func TestRead(t *testing.T) {
	out := map[string]string{"--json": scanJSON, "/dev/sda": ssdJSON, "/dev/sdb": hddJSON, "/dev/nvme0": nvmeJSON, "/dev/sdc": standbyJSON}
	r, calls := fakeReader(out)
	s := r.Read(context.Background())
	if s.Error != "" || len(s.Disks) != 4 {
		t.Fatalf("stand: %+v", s)
	}
	for _, c := range *calls {
		if strings.Contains(c, "reboot") {
			t.Fatalf("ungültiges gerät abgefragt: %q", c)
		}
	}
	if (*calls)[1] != "-a --json -n standby -d sat /dev/sda" {
		t.Errorf("aufruf: %q", (*calls)[1])
	}
	ssd, hdd, nvme, sleeping := s.Disks[0], s.Disks[1], s.Disks[2], s.Disks[3]
	if ssd.Model != "Samsung SSD 870 EVO 1TB" || !*ssd.Passed || *ssd.TempC != 31 || *ssd.PowerOnHrs != 8123 ||
		*ssd.Reallocated != 0 || *ssd.CRCErrors != 2 || ssd.Problem() != "" {
		t.Errorf("ssd: %+v", ssd)
	}
	if *hdd.Reallocated != 24 || hdd.Problem() != "8 ausstehende Sektoren" {
		t.Errorf("hdd: %+v %q", hdd, hdd.Problem())
	}
	if *nvme.PercentUsed != 3 || *nvme.AvailableSpare != 100 || nvme.Reallocated != nil || nvme.Problem() != "" {
		t.Errorf("nvme: %+v", nvme)
	}
	if !sleeping.Standby || sleeping.Passed != nil || sleeping.Device != "/dev/sdc" {
		t.Errorf("standby ohne vorwert: %+v", sleeping)
	}

	// Zwischengespeichert: kein neuer Lauf innerhalb der TTL.
	n := len(*calls)
	r.Read(context.Background())
	if len(*calls) != n {
		t.Error("smartctl innerhalb der ttl erneut aufgerufen")
	}
	// Schläft die SSD beim nächsten Lauf, bleiben ihre Werte (markiert) stehen.
	out["/dev/sda"] = standbyJSON
	r.at = r.at.Add(-TTL)
	s = r.Read(context.Background())
	if d := s.Disks[0]; !d.Standby || d.Model != "Samsung SSD 870 EVO 1TB" || *d.TempC != 31 {
		t.Errorf("standby mit vorwert: %+v", d)
	}
}

func TestMissing(t *testing.T) {
	s := (&Reader{last: map[string]proto.SmartDisk{}}).Read(context.Background())
	if s.Error != ErrMissing.Error() || s.Disks != nil {
		t.Fatalf("ohne smartctl: %+v", s)
	}
}

func TestFailedAndCritical(t *testing.T) {
	d, _, err := parseDevice([]byte(`{"smartctl":{"exit_status":8},"model_name":"X","smart_status":{"passed":false}}`))
	if err != nil || d.Problem() != "SMART-Bewertung: FAILED" {
		t.Fatalf("failed: %+v %v", d, err)
	}
	d, _, _ = parseDevice([]byte(`{"model_name":"Y","smart_status":{"passed":true},"nvme_smart_health_information_log":{"critical_warning":4}}`))
	if !strings.Contains(d.Problem(), "0x04") {
		t.Errorf("kritisch: %q", d.Problem())
	}
	if _, _, err := parseDevice([]byte(`{"smartctl":{"exit_status":2}}`)); err == nil {
		t.Error("nicht lesbares gerät akzeptiert")
	}
}
