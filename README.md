# janitor
## Objective
Janitor is a standalone tool that monitors the availability of your IOT devices and alerts you in case a device goes missing or stops transmitting data. This is particulary useful if you have many sensors, possibly with unstable hardware or connection, so you can take action in case of any issues and monitor the stability of your devices.

Janitor does not aim to implement any additional functionalities, therefore is not an alternative to your other home automation software (e.g. HASS). Focusing on solely this functionality will enable to keep this tool simple and efficient.

Janitor currently supports the following monitoring methods:
* **MQTT:** Janitor will subscribe to predefined MQTT topics and monitor incoming messages. An average transmit frequency will be established for each channel and in case no new messages are received within this interval, Janitor will alert you. This method is particulary useful for any kind of sensors submitting data regularly via MQTT (e.g. temperature).
* **Ping:** Janitor will ping predefined hosts with a predefined frequency and will alert you in case of no reply. This method is useful for any kind of IOT devices e.g. sensors, cameras etc.

Janitor currently supports the following alert methods:
* **Telegram:** Janitor will send a message to a predefined Telegram channel.

Additionally, Janitor has a web interface where you can see the current status and historical data, remove sensors and reload the configuration file (see screenshot below).

## Screenshot
## Building and installing
## Configuration and usage
## Future plans and contributing
## License
