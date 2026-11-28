"""Constants for the YouTube Music Pulsar integration."""

DOMAIN = "ytm_pulsar"

CONF_HOST = "host"
CONF_PORT = "port"
CONF_URL = "url"

DEFAULT_PORT = 24721

INTENT_PLAY = "YtmPulsarPlay"
INTENT_NEXT = "YtmPulsarNext"
INTENT_PREVIOUS = "YtmPulsarPrevious"
INTENT_SEARCH = "YtmPulsarSearch"
INTENT_CLEAR = "YtmPulsarClear"
INTENT_RANDOM = "YtmPulsarRandom"
INTENT_FORWARD = "YtmPulsarForward"
INTENT_REWIND = "YtmPulsarRewind"
INTENT_SPEAKER_ON = "YtmPulsarSpeakerOn"
INTENT_SPEAKER_OFF = "YtmPulsarSpeakerOff"

INTENTS = (
    INTENT_PLAY,
    INTENT_NEXT,
    INTENT_PREVIOUS,
    INTENT_SEARCH,
    INTENT_CLEAR,
    INTENT_RANDOM,
    INTENT_FORWARD,
    INTENT_REWIND,
    INTENT_SPEAKER_ON,
    INTENT_SPEAKER_OFF,
)

SENTENCE_FILE = "ytm_pulsar.yaml"
SENTENCE_MARKER = "# ytm-pulsar voice sentences"
