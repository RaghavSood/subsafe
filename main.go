package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type ChainConfig struct {
	Name           string `json:"name"`
	ChainID        int64  `json:"chain_id"`
	RPC            string `json:"rpc_url"`
	ExplorerURL    string `json:"explorer_url"`
	SafeServiceURL string `json:"safe_service_url"`
}

type SafeTransactionResponse struct {
	Proposer      string `json:"proposer"`
	Confirmations []struct {
		Owner          string    `json:"owner"`
		SubmissionDate time.Time `json:"submissionDate"`
		SignatureType  string    `json:"signatureType"`
	} `json:"confirmations"`
}

type Config struct {
	TelegramToken string            `json:"telegram_token"`
	ChatID        int64             `json:"chat_id"`
	SafeAddresses map[string]string `json:"safe_addresses"`
	Chains        []ChainConfig     `json:"chains"`
}

type Monitor struct {
	config     Config
	bot        *tgbotapi.BotAPI
	safeABI    abi.ABI
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

func getSafeTransactionDetails(serviceURL, txHash string) (*SafeTransactionResponse, error) {
	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	// Build the API URL
	url := fmt.Sprintf("%s/api/v1/multisig-transactions/%s/", serviceURL, txHash)

	// Make the request
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to call Safe API: %w", err)
	}
	defer resp.Body.Close()

	// Check response status
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Safe API returned non-200 status: %d, body: %s", resp.StatusCode, string(body))
	}

	// Parse the response
	var result SafeTransactionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode Safe API response: %w", err)
	}

	return &result, nil
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

	ctx, cancel := context.WithCancel(context.Background())

	return &Monitor{
		config:     config,
		bot:        bot,
		safeABI:    parsedABI,
		ctx:        ctx,
		cancelFunc: cancel,
	}, nil
}

func (m *Monitor) sendMessage(msg string) {
	teleMsg := tgbotapi.NewMessage(m.config.ChatID, msg)
	teleMsg.ParseMode = "MarkdownV2"
	teleMsg.DisableWebPagePreview = true
	if _, err := m.bot.Send(teleMsg); err != nil {
		log.Printf("Error sending telegram message: %v", err)
	}
}

func (m *Monitor) monitorChain(chainConfig ChainConfig, addresses []common.Address, addressLabels map[common.Address]string) {
	defer m.wg.Done()

	const (
		initialRetryDelay = 1 * time.Second
		maxRetryDelay     = 60 * time.Second
		backoffFactor     = 2
		maxFailedAttempts = 3
	)

	retryDelay := initialRetryDelay
	firstConnect := true
	attemptCount := 0
	alertSent := false

	for {
		if !firstConnect {
			time.Sleep(retryDelay)
			retryDelay *= backoffFactor
			if retryDelay > maxRetryDelay {
				retryDelay = maxRetryDelay
			}
		}

		if err := m.ctx.Err(); err != nil {
			return // Context cancelled
		}

		// Only send connecting message on first attempt or after alert threshold
		if firstConnect || (attemptCount >= maxFailedAttempts) {
			chainName := strings.ReplaceAll(chainConfig.Name, ".", "\\.")
			m.sendMessage(fmt.Sprintf("🔄 Connecting to *%s* \\(Chain ID: %d\\)", chainName, chainConfig.ChainID))
		}

		// Create new client for each attempt
		client, err := ethclient.Dial(chainConfig.RPC)
		if err != nil {
			attemptCount++
			if attemptCount >= maxFailedAttempts && !alertSent {
				errorMsg := strings.ReplaceAll(err.Error(), ".", "\\.")
				chainName := strings.ReplaceAll(chainConfig.Name, ".", "\\.")
				m.sendMessage(fmt.Sprintf("⚠️ *ALERT*: %s connection has failed %d times in a row\\. This could indicate a serious connectivity issue\\.\nLast error: %s",
					chainName, attemptCount, errorMsg))
				alertSent = true
			} else {
				m.sendMessage(fmt.Sprintf("❌ Failed to connect to %s: %v. Retrying in %v...",
					chainConfig.Name, err, retryDelay))
			}
			continue
		}

		logs := make(chan types.Log)
		sub, err := client.SubscribeFilterLogs(m.ctx, ethereum.FilterQuery{
			Addresses: addresses,
		}, logs)

		if err != nil {
			attemptCount++
			if attemptCount >= maxFailedAttempts && !alertSent {
				m.sendMessage(fmt.Sprintf("⚠️ ALERT: %s connection has failed %d times in a row. This could indicate a serious connectivity issue.\nLast error: %v",
					chainConfig.Name, attemptCount, err))
				alertSent = true
			} else {
				m.sendMessage(fmt.Sprintf("❌ Failed to subscribe to logs on %s: %v. Retrying in %v...",
					chainConfig.Name, err, retryDelay))
			}
			client.Close()
			continue
		}

		// Successfully connected - reset counters
		if firstConnect || alertSent {
			chainName := strings.ReplaceAll(chainConfig.Name, ".", "\\.")
			m.sendMessage(fmt.Sprintf("🟢 Successfully connected to *%s*", chainName))
		}
		retryDelay = initialRetryDelay
		attemptCount = 0
		alertSent = false
		firstConnect = false

		func() {
			defer client.Close()
			defer sub.Unsubscribe()

			for {
				select {
				case err := <-sub.Err():
					log.Printf("Error from subscription: %v", err)
					return
				case vLog := <-logs:
					event, err := m.safeABI.EventByID(vLog.Topics[0])
					if err != nil {
						log.Printf("Error parsing event: %v", err)
						continue
					}

					txHash := vLog.TxHash.Hex()
					walletLabel := addressLabels[vLog.Address]
					// Escape special characters for MarkdownV2
					chainName := strings.ReplaceAll(chainConfig.Name, ".", "\\.")
					eventName := strings.ReplaceAll(event.Name, ".", "\\.")
					escapedLabel := strings.ReplaceAll(walletLabel, ".", "\\.")
					address := strings.ReplaceAll(vLog.Address.Hex(), ".", "\\.")
					txHashEscaped := strings.ReplaceAll(txHash, ".", "\\.")
					explorerURL := strings.ReplaceAll(fmt.Sprintf("%s/tx/%s", chainConfig.ExplorerURL, txHash), ".", "\\.")

					var signerInfo string
					if eventName == "ExecutionSuccess" && len(vLog.Topics) > 1 {
						safeTxHash := vLog.Topics[1].Hex()
						// Try to get Safe transaction details
						if chainConfig.SafeServiceURL != "" {
							if txDetails, err := getSafeTransactionDetails(chainConfig.SafeServiceURL, safeTxHash); err != nil {
								signerInfo = "\n\\(Safe API call failed\\)"
							} else {
								proposer := strings.ReplaceAll(txDetails.Proposer[:6], ".", "\\.")
								signers := make([]string, 0, len(txDetails.Confirmations))
								for _, conf := range txDetails.Confirmations {
									signer := strings.ReplaceAll(conf.Owner[:6], ".", "\\.")
									sigType := strings.ReplaceAll(string(conf.SignatureType), ".", "\\.")
									signers = append(signers, fmt.Sprintf("`%s` \\(%s\\)", signer, sigType))
								}
								signerInfo = fmt.Sprintf("\n*Proposer:* `%s`\n*Signers:* %s",
									proposer, strings.Join(signers, ", "))
							}
						}
					}

					msg := fmt.Sprintf("🔔 *New event on %s*\n"+
						"*Wallet:* %s\n"+
						"*Type:* %s\n"+
						"*Address:* `%s`\n"+
						"*Tx:* [%s](%s)%s",
						chainName, escapedLabel, eventName, address, txHashEscaped, explorerURL, signerInfo)
					m.sendMessage(msg)
				case <-m.ctx.Done():
					return
				}
			}
		}()
	}
}

func (m *Monitor) Start() error {
	// Create slice of addresses and map of labels
	addresses := make([]common.Address, 0, len(m.config.SafeAddresses))
	addressLabels := make(map[common.Address]string)

	for label, addr := range m.config.SafeAddresses {
		address := common.HexToAddress(addr)
		addresses = append(addresses, address)
		addressLabels[address] = label
	}

	for _, chainConfig := range m.config.Chains {
		m.wg.Add(1)
		go m.monitorChain(chainConfig, addresses, addressLabels)
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
