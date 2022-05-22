package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	nostr "github.com/fiatjaf/go-nostr"
	_ "github.com/go-sql-driver/mysql"
	"github.com/robfig/cron/v3"
	"github.com/spf13/viper"
)

const ROBOSATS_PUBLIC_ORDERS_QUERY = "/api/book/?currency=0&type=2"

var config AppConfig
var pool nostr.RelayPool
var torClient *http.Client

type AppConfig struct {
	TorProxyUrl         string
	TorProxyPort        string
	NostrPrivkey        string
	NostrRelays         string
	RobosatsOnionUrl    string
	RobosatsReferralUrl string
	DbUrl               string
	DbTable             string
	DbUsername          string
	DbPassword          string
}

type Order struct {
	Id             int     `json:"id"`
	CreatedAt      string  `json:"created_at"`
	ExpiresAt      string  `json:"expires_at"`
	Type           int     `json:"type"`
	Currency       int     `json:"currency"`
	Amount         string  `json:"amount"`
	HasRange       bool    `json:"has_range"`
	MinAmount      string  `json:"min_amount"`
	MaxAmount      string  `json:"max_amount"`
	PaymentMethod  string  `json:"payment_method"`
	IsExplicit     bool    `json:"is_explicit"`
	Premium        float32 `json:"premium"`
	Satoshis       string  `json:"satoshis"`
	BondlessTaker  bool    `json:"bondless_taker"`
	Maker          int     `json:"maker"`
	EscrowDuration int     `json:"escrow_duration"`
	MakerNick      string  `json:"maker_nick"`
	Price          float32 `json:"price"`
	MakerStatus    string  `json:"maker_status"`
}

// Robosats currencies, https://github.com/Reckless-Satoshi/robosats/blob/main/frontend/static/assets/currencies.json
var currencies = map[int]string{
	1:    "USD",
	2:    "EUR",
	3:    "JPY",
	4:    "GBP",
	5:    "AUD",
	6:    "CAD",
	7:    "CHF",
	8:    "CNY",
	9:    "HKD",
	10:   "NZD",
	11:   "SEK",
	12:   "KRW",
	13:   "SGD",
	14:   "NOK",
	15:   "MXN",
	16:   "KRW",
	17:   "RUB",
	18:   "ZAR",
	19:   "TRY",
	20:   "BRL",
	21:   "CLP",
	22:   "CZK",
	23:   "DKK",
	24:   "HRK",
	25:   "HUF",
	26:   "INR",
	27:   "ISK",
	28:   "PLN",
	29:   "RON",
	30:   "ARS",
	31:   "VES",
	32:   "COP",
	33:   "PEN",
	34:   "UYU",
	35:   "PYG",
	36:   "BOB",
	37:   "IDR",
	38:   "ANG",
	39:   "CRC",
	40:   "CUP",
	41:   "DOP",
	42:   "GHS",
	43:   "GTQ",
	44:   "ILS",
	45:   "JMD",
	46:   "KES",
	47:   "KZT",
	48:   "MYR",
	49:   "NAD",
	50:   "NGN",
	51:   "AZN",
	52:   "PAB",
	53:   "PHP",
	54:   "PKR",
	55:   "QAR",
	56:   "SAR",
	57:   "THB",
	58:   "TTD",
	59:   "VND",
	60:   "XOF",
	61:   "TWD",
	62:   "TZS",
	63:   "XAF",
	64:   "UAH",
	300:  "XAU",
	1000: "BTC",
}

func main() {
	// Read config file
	initConfig()
	// Initialize nostr pool connection(s)
	initPool()
	// Init Tor client
	initTorClient()

	c := cron.New()
	c.AddFunc("@every 5m", updateOrders)
	c.Start()
	defer c.Stop()
	select {}
}

func updateOrders() {
	// Init database
	db := initDb()
	defer db.Close()

	// Get active RoboSats orders
	orders := getOrderbook()

	// Loop over all active orders and post new ones
	for _, order := range orders {
		var id int
		err := db.QueryRow("SELECT orderId FROM orders where orderId = ?", order.Id).Scan(&id)
		if err != nil {
			// Does not already exist in db
			if err == sql.ErrNoRows {
				err := insertOrder(order.Id, db)
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s", err)
					os.Exit(1)
				}
				postNewOrderToNostr(order)
			} else {
				fmt.Fprintf(os.Stderr, "%s", err)
				os.Exit(1)
			}
		}
	}

}

// Initializes Viper configuration
func initConfig() {
	viper.SetConfigName("config")
	viper.AddConfigPath(".")
	viper.SetConfigType("yml")

	viper.WatchConfig()

	if err := viper.ReadInConfig(); err != nil {
		fmt.Fprintf(os.Stderr, "\nError reading config file\n")
		os.Exit(1)
	}
	err := viper.Unmarshal(&config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nUnable to decode config file into struct\n")
		os.Exit(1)
	}

	if config.NostrRelays == "" {
		fmt.Fprintf(os.Stderr, "\nNo relays found in config. You must configure at least one relay\n")
		os.Exit(1)
	}

	if config.NostrPrivkey == "" {
		fmt.Printf("Nostr privatekey missing, can't continue\n")
		os.Exit(1)
	} else {
		pubkey, _ := nostr.GetPublicKey(config.NostrPrivkey)
		fmt.Printf("Using nostr pubkey %s\n", pubkey)
	}

	if config.RobosatsReferralUrl == "" {
		fmt.Fprint(os.Stderr, "\nRobosats referral url is missing from config\n")
		os.Exit(1)
	}

	if config.DbUrl == "" {
		fmt.Fprint(os.Stderr, "\nDatabase url is missing from config\n")
		os.Exit(1)
	}
	if config.DbTable == "" {
		fmt.Fprint(os.Stderr, "\nDatabase table is missing from config\n")
		os.Exit(1)
	}
	if config.DbUsername == "" {
		fmt.Fprint(os.Stderr, "\nDatabase username is missing from config\n")
		os.Exit(1)
	}
	if config.DbPassword == "" {
		fmt.Fprint(os.Stderr, "\nDatabase password is missing from config\n")
		os.Exit(1)
	}
}

// Initializes nostr connection(s)
func initPool() {
	pool = *nostr.NewRelayPool()
	pool.SecretKey = &config.NostrPrivkey

	relays := strings.Split(config.NostrRelays, ",")

	for _, relay := range relays {
		pool.Add(relay, nil)
	}
}

func initDb() *sql.DB {
	datasourcename := fmt.Sprintf("%s:%s@tcp(%s)/%s", config.DbUsername, config.DbPassword, config.DbUrl, config.DbTable)
	db, err := sql.Open("mysql", datasourcename)
	if err != nil {
		panic(err)
	}

	db.SetConnMaxLifetime(time.Minute * 3)
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(10)

	return db
}

func initTorClient() {
	// Parse Tor proxy URL string to a URL type
	torurl := fmt.Sprintf("%s:%s", config.TorProxyUrl, config.TorProxyPort)
	torProxyUrl, err := url.Parse(torurl)

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing Tor proxy URL: %s. \n%s\n", torProxyUrl, err)
		os.Exit(1)
	}

	// Set up a custom HTTP transport to use the proxy and create the client
	torTransport := &http.Transport{Proxy: http.ProxyURL(torProxyUrl)}
	torClient = &http.Client{Transport: torTransport, Timeout: time.Second * 30}
}

func postNewOrderToNostr(order Order) {
	var orderType string
	if order.Type == 0 {
		orderType = "BUY"
	} else if order.Type == 1 {
		orderType = "SELL"
	} else {
		orderType = "UNKNOWN"
	}

	var amount string
	if order.HasRange {
		minAmount, err := strconv.ParseFloat(order.MinAmount, 64)
		if err != nil {
			fmt.Println("Cannot parse min amount")
		}
		maxAmount, err := strconv.ParseFloat(order.MaxAmount, 64)
		if err != nil {
			fmt.Println("Cannot parse max amount")
		}

		amount = fmt.Sprintf("%0.f-%0.f", minAmount, maxAmount)
	} else {
		amt, err := strconv.ParseFloat(order.Amount, 64)
		if err != nil {
			fmt.Println("Cannot parse amount")
		}

		amount = fmt.Sprintf("%0.f", amt)
	}

	currency, exists := currencies[order.Currency]
	if exists == false {
		currency = fmt.Sprintf("Unknown currency (%d)", order.Currency)
	}

	premium := fmt.Sprintf("%0.1f%%", order.Premium)

	price := fmt.Sprintf("%0.f", order.Price)

	content := fmt.Sprintf("Type: %s\nAmount: %s\nCurrency: %s\nPayment method: %s\nPremium: %s\nPrice: %s\nLINK(TOR): %s", orderType, amount, currency, order.PaymentMethod, premium, price, config.RobosatsReferralUrl)

	event, _, _ := pool.PublishEvent(&nostr.Event{
		CreatedAt: time.Now(),
		Kind:      nostr.KindTextNote,
		Tags:      make(nostr.Tags, 0),
		Content:   content,
	})

	fmt.Printf("Sent event %s for order id %d\n", event.ID, order.Id)
}

func getOrderbook() []Order {
	fmt.Println("Fetching orders...")
	resp, err := torClient.Get(config.RobosatsOnionUrl + ROBOSATS_PUBLIC_ORDERS_QUERY)
	if err != nil {
		fmt.Println(err)
	}
	defer resp.Body.Close()

	body, _ := ioutil.ReadAll(resp.Body)
	fmt.Printf("\nGot body: \n%s\n", string(body))

	var orders []Order
	if err := json.Unmarshal(body, &orders); err != nil {
		fmt.Println(err)
	}

	fmt.Printf("\nFound %d orders\n", len(orders))
	return orders
}

func insertOrder(orderId int, db *sql.DB) error {
	insert, err := db.Query("INSERT INTO orders VALUES (?)", orderId)
	if err != nil {
		return err
	}
	defer insert.Close()
	fmt.Printf("\nOrder added to database: %d\n", orderId)
	return nil
}
