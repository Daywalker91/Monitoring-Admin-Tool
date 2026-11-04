// Package pawnio liest unter Windows die CPU-Temperatur über den Treiber PawnIO
// (https://pawnio.eu). Windows selbst hat dafür keine Schnittstelle; ohne einen
// Kerneltreiber kommt man an die Sensoren der CPU nicht heran.
//
// Sicherheit: Den Treiber installiert der Administrator selbst am Host (nur die
// signierte Ausgabe); der Agent installiert nichts und lädt nur die beiden
// signierten Module aus modules/. Aufgerufen werden ausschließlich die
// Lesefunktionen ioctl_read_msr (Intel) und ioctl_read_smn (AMD) mit fest
// eingetragenen Registern; nichts davon kommt vom Server. Fehlt der Treiber oder
// passt die CPU nicht, gibt es einfach keinen Wert.
//
// Diese Datei enthält nur die Umrechnung (auf allen Plattformen testbar).
package pawnio

import "strings"

// Intel-MSRs (Intel SDM Bd. 4).
const (
	msrThermStatus        = 0x19C // IA32_THERM_STATUS, je Kern
	msrTemperatureTarget  = 0x1A2 // MSR_TEMPERATURE_TARGET (TjMax)
	msrPackageThermStatus = 0x1B1 // IA32_PACKAGE_THERM_STATUS, ganze CPU
)

// AMD Family 17h–1Ah: Register THM_TCON_CUR_TMP im System Management Network.
const smnThmTconCurTmp = 0x00059800

// tjMax liest TjMax aus MSR_TEMPERATURE_TARGET (Bits 23:16); ältere CPUs
// melden dort 0, dann gilt 100 °C wie bei Linux (coretemp).
func tjMax(target uint64) float64 {
	if v := (target >> 16) & 0xFF; v != 0 {
		return float64(v)
	}
	return 100
}

// intelTemp rechnet IA32_(PACKAGE_)THERM_STATUS um: Die Bits 22:16 sind der
// Abstand zu TjMax in °C. valid verlangt Bit 31 ("Reading Valid"), das es nur
// im Kern-Register gibt.
func intelTemp(tj float64, status uint64, valid bool) (float64, bool) {
	if valid && status&(1<<31) == 0 {
		return 0, false
	}
	return tj - float64((status>>16)&0x7F), true
}

// amdTemp rechnet THM_TCON_CUR_TMP um wie k10temp unter Linux: Bits 31:21 in
// 0,125 °C; ist der erweiterte Bereich gewählt (Bit 19 oder Bits 17:16 = 11),
// liegt der Wert 49 °C zu hoch. offset gleicht Tctl einiger Modelle auf die
// echte Temperatur (Tdie) an.
func amdTemp(raw uint32, offset float64) float64 {
	t := float64(raw>>21) * 0.125
	if raw&(1<<19) != 0 || raw&0x30000 == 0x30000 {
		t -= 49
	}
	return t + offset
}

// amdOffset: Bei diesen Modellen ist Tctl absichtlich höher als die echte
// Temperatur (Tabelle aus k10temp bzw. LibreHardwareMonitor).
func amdOffset(name string) float64 {
	switch {
	case strings.Contains(name, "1600X"), strings.Contains(name, "1700X"), strings.Contains(name, "1800X"):
		return -20
	case strings.Contains(name, "2700X"):
		return -10
	case strings.Contains(name, "Threadripper 19"), strings.Contains(name, "Threadripper 29"):
		return -27
	}
	return 0
}

// plausible verwirft offensichtlich falsche Werte (z. B. 0 von einem
// nicht unterstützten Register).
func plausible(c float64) bool { return c > 0 && c < 150 }

// execInput baut den Eingabepuffer für "Funktion ausführen": Name mit NUL auf
// 32 Byte, danach die Argumente als 64-Bit-Werte (little endian).
func execInput(fn string, args ...uint64) []byte {
	const nameLen = 32
	buf := make([]byte, nameLen+8*len(args))
	copy(buf[:nameLen-1], fn)
	for i, a := range args {
		for b := range 8 {
			buf[nameLen+8*i+b] = byte(a >> (8 * b))
		}
	}
	return buf
}

func le64(b []byte) uint64 {
	var v uint64
	for i := range 8 {
		v |= uint64(b[i]) << (8 * i)
	}
	return v
}
