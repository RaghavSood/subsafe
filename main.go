package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type ChainConfig struct {
	ChainID     int64  `json:"chain_id"`
	RPC         string `json:"rpc_url"`
	ExplorerURL string `json:"explorer_url"`
}

type Config struct {
	TelegramToken string        `json:"telegram_token"`
	ChatID        int64         `json:"chat_id"`
	SafeAddresses []string      `json:"safe_addresses"`
	Chains        []ChainConfig `json:"chains"`
}

type Monitor struct {
	config     Config
	bot        *tgbotapi.BotAPI
	safeABI    abi.ABI
	clients    map[int64]*ethclient.Client
	wg         sync.WaitGroup
	ctx        context.Context
	cancelFunc context.CancelFunc
}

func loadConfig(path string) (Config, error) {
	var config Config
	data, err := os.ReadFile(path)
	if err != nil {
		return config, fmt.Errorf("reading config file: %w", err)
	}

	if err := json.Unmarshal(data, &config); err != nil {
		return config, fmt.Errorf("parsing config: %w", err)
	}

	return config, nil
}

func NewMonitor(config Config) (*Monitor, error) {
	bot, err := tgbotapi.NewBotAPI(config.TelegramToken)
	if err != nil {
		return nil, fmt.Errorf("creating telegram bot: %w", err)
	}

	parsedABI, err := abi.JSON(strings.NewReader(gnosisSafeABI))
	if err != nil {
		return nil, fmt.Errorf("parsing ABI: %w", err)
	}

	clients := make(map[int64]*ethclient.Client)
	for _, chain := range config.Chains {
		client, err := ethclient.Dial(chain.RPC)
		if err != nil {
			return nil, fmt.Errorf("connecting to chain %d: %w", chain.ChainID, err)
		}
		clients[chain.ChainID] = client
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Monitor{
		config:     config,
		bot:        bot,
		safeABI:    parsedABI,
		clients:    clients,
		ctx:        ctx,
		cancelFunc: cancel,
	}, nil
}

func (m *Monitor) sendMessage(msg string) {
	teleMsg := tgbotapi.NewMessage(m.config.ChatID, msg)
	if _, err := m.bot.Send(teleMsg); err != nil {
		log.Printf("Error sending telegram message: %v", err)
	}
}

func (m *Monitor) monitorChain(chainID int64, client *ethclient.Client, addresses []common.Address) {
	defer m.wg.Done()

	chainConfig := func() ChainConfig {
		for _, c := range m.config.Chains {
			if c.ChainID == chainID {
				return c
			}
		}
		return ChainConfig{}
	}()

	m.sendMessage(fmt.Sprintf("🟢 Started monitoring chain %d", chainID))

	logs := make(chan types.Log)
	sub, err := client.SubscribeFilterLogs(m.ctx, ethereum.FilterQuery{
		Addresses: addresses,
	}, logs)

	if err != nil {
		m.sendMessage(fmt.Sprintf("❌ Failed to subscribe to logs on chain %d: %v", chainID, err))
		return
	}
	defer sub.Unsubscribe()

	for {
		select {
		case err := <-sub.Err():
			m.sendMessage(fmt.Sprintf("❌ Lost connection to chain %d: %v", chainID, err))
			return
		case vLog := <-logs:
			event, err := m.safeABI.EventByID(vLog.Topics[0])
			if err != nil {
				log.Printf("Error parsing event: %v", err)
				continue
			}

			txURL := fmt.Sprintf("%s/tx/%s", chainConfig.ExplorerURL, vLog.TxHash.Hex())
			msg := fmt.Sprintf("🔔 New event on chain %d\n"+
				"Type: %s\n"+
				"Address: %s\n"+
				"Transaction: %s",
				chainID, event.Name, vLog.Address.Hex(), txURL)
			m.sendMessage(msg)
		case <-m.ctx.Done():
			return
		}
	}
}

func (m *Monitor) Start() error {
	addresses := make([]common.Address, len(m.config.SafeAddresses))
	for i, addr := range m.config.SafeAddresses {
		addresses[i] = common.HexToAddress(addr)
	}

	for chainID, client := range m.clients {
		m.wg.Add(1)
		go m.monitorChain(chainID, client, addresses)
	}

	// Setup graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	<-sigChan
	m.sendMessage("⚠️ Bot is shutting down")
	m.cancelFunc()
	m.wg.Wait()

	return nil
}

func main() {
	if len(os.Args) != 2 {
		log.Fatal("Usage: ./safe-monitor config.json")
	}

	config, err := loadConfig(os.Args[1])
	if err != nil {
		log.Fatalf("Error loading config: %v", err)
	}

	monitor, err := NewMonitor(config)
	if err != nil {
		log.Fatalf("Error creating monitor: %v", err)
	}

	if err := monitor.Start(); err != nil {
		log.Fatalf("Error running monitor: %v", err)
	}
}
