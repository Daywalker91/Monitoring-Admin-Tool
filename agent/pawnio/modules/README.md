# PawnIO-Module

Unveränderte, signierte Module aus dem offiziellen Release von
[PawnIO.Modules](https://github.com/namazso/PawnIO.Modules) (Release 0.2.11, `release_0_2_11.zip`).
Lizenz: LGPL-2.1-or-later (siehe `COPYING`), Quellcode im genannten Repository.

| Datei | SHA-256 |
|-------|---------|
| `IntelMSR.bin` | `d6ed85d65ab17a22f813ef98207d6d537155ee2ded5976a21cb48413c9b92e5f` |
| `AMDFamily17.bin` | `dae74615761b78bdf064dfb3e136252ddcc6fc727d88f14738d0e5800d427a91` |

Der Agent ruft daraus nur `ioctl_read_msr` (Intel) bzw. `ioctl_read_smn` (AMD) mit fest eingetragenen
Registern auf. Aktualisieren: neues Release laden, beide Dateien ersetzen, Tabelle anpassen.
