// idevicewifi: read or set lockdown EnableWifiConnections (Finder's "Show this iPhone when on Wi-Fi").
// libimobiledevice ships no CLI that writes this value.
#include <stdio.h>
#include <string.h>
#include <libimobiledevice/libimobiledevice.h>
#include <libimobiledevice/lockdown.h>

#define DOMAIN "com.apple.mobile.wireless_lockdown"
#define KEY "EnableWifiConnections"

int main(int argc, char **argv)
{
	const char *udid = NULL, *cmd = NULL;
	for (int i = 1; i < argc; i++) {
		if (!strcmp(argv[i], "-u") && i + 1 < argc) udid = argv[++i];
		else cmd = argv[i];
	}
	if (!cmd || (strcmp(cmd, "on") && strcmp(cmd, "off") && strcmp(cmd, "status"))) {
		fprintf(stderr, "usage: %s [-u UDID] on|off|status\n", argv[0]);
		return 2;
	}

	idevice_t dev = NULL;
	if (idevice_new_with_options(&dev, udid, IDEVICE_LOOKUP_USBMUX) != IDEVICE_E_SUCCESS) {
		fprintf(stderr, "no device found\n");
		return 1;
	}
	lockdownd_client_t ld = NULL;
	lockdownd_error_t err = lockdownd_client_new_with_handshake(dev, &ld, "idevicewifi");
	if (err != LOCKDOWN_E_SUCCESS) {
		fprintf(stderr, "lockdown handshake failed: %d (is the device paired?)\n", err);
		idevice_free(dev);
		return 1;
	}

	int rc = 0;
	if (!strcmp(cmd, "status")) {
		plist_t val = NULL;
		uint8_t on = 0;
		if (lockdownd_get_value(ld, DOMAIN, KEY, &val) == LOCKDOWN_E_SUCCESS && val) {
			plist_get_bool_val(val, &on);
			plist_free(val);
		}
		printf("%s\n", on ? "on" : "off");
	} else {
		err = lockdownd_set_value(ld, DOMAIN, KEY, plist_new_bool(!strcmp(cmd, "on")));
		if (err != LOCKDOWN_E_SUCCESS) {
			fprintf(stderr, "set failed: %d\n", err);
			rc = 1;
		} else {
			printf("%s\n", cmd);
		}
	}
	lockdownd_client_free(ld);
	idevice_free(dev);
	return rc;
}
