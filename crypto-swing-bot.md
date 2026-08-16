# Kripto Swing Trading Botu — Teknik Şartname ve Yol Haritası

> **Uyarı:** Bu doküman bir yazılım mimarisi şartnamesidir. Yatırım tavsiyesi içermez.
> İçindeki hiçbir strateji, parametre veya örnek kârlılık vaadi değildir. Sistem, bir
> stratejinin kâr ettiğini varsaymak için değil, **kâr edip etmediğini dürüstçe ölçmek**
> için tasarlanmıştır.

**Sürüm:** 1.0
**Kapsam:** Tek kullanıcı (geliştiricinin kendisi), kendi sermayesi, spot piyasa, günlük/haftalık (swing) vade
**Dil:** Go 1.22+
**Durum:** Faz 0 başlangıcı

---

## 0. Bu doküman nasıl kullanılır

Bu dosya projenin tek referans kaynağıdır. VS Code'da repo kökünde `SPEC.md` olarak tut.

- Bir modülü kodlamaya başlamadan önce ilgili bölümü oku, sonuna kadar oku, sonra yaz.
- Kod snippet'leri **sözleşmedir**, kopyala-yapıştır kütüphanesi değil. İmzalar ve
  sorumluluk sınırları bağlayıcıdır; gövde implementasyonu sana ait.
- Bir tasarım kararını değiştirirsen bu dosyayı da güncelle. Doküman ile kod ayrışırsa
  doküman ölür ve proje kontrolden çıkar.
- Bir yapay zekâ kodlama asistanı kullanacaksan: ilgili bölümün tamamını bağlama ver,
  "şu bölümdeki `X` arayüzünü implemente et" de. Tüm dosyayı tek seferde vermek
  bağlamı sulandırır.
- Faz kabul kriterlerini (bkz. Bölüm 12) atlamadan ilerle. Her fazın sonunda kriterler
  sağlanmıyorsa bir sonraki faza geçme.

### Bağımlılık sürümleri hakkında

Aşağıda önerilen kütüphanelerin API'leri zamanla değişir. `go get` sonrası
`pkg.go.dev` üzerinden güncel imzayı doğrula. Bu dokümandaki kullanım örnekleri
kavramsaldır.

---

## 1. Proje özeti ve tasarım ilkeleri

### 1.1 Sistem ne yapar

Günde bir kez (UTC 00:00 günlük mum kapanışında) çalışır:

1. Borsadan güncel günlük mumları çeker.
2. İşlem yapılabilir coin evrenini filtreler ve skorlar.
3. Aktif stratejiyi çalıştırıp giriş/çıkış sinyalleri üretir.
4. Risk kurallarından geçen sinyalleri **öneri** haline getirir.
5. Öneriyi Telegram üzerinden kullanıcıya sunar ve onay bekler.
6. Onaylanan öneriyi emre çevirip borsaya gönderir.
7. Açık pozisyonların stop seviyelerini günceller, çıkış koşullarını kontrol eder.
8. Her şeyi kaydeder; yerel web panelinde gösterir.

### 1.2 Bağlayıcı tasarım ilkeleri

Bu ilkeler pazarlık konusu değildir. Her biri, bu tür projelerin battığı somut bir
noktaya karşılık gelir.

**İ1 — Tek kod yolu.** Backtest, paper trading ve canlı işlem **aynı** strateji ve risk
kodunu çalıştırır. Fark yalnızca `Clock`, `DataSource` ve `Broker` implementasyonlarındadır.
Backtest için ayrı bir strateji kodu yazarsan canlıda ölçtüğün şey test ettiğin şey olmaz.

**İ2 — Sinyal `t`, işlem `t+1`.** `t` gününün kapanışında oluşan sinyal, en erken `t+1`
gününün açılışında gerçekleşir. Bu kural motorun içine gömülüdür, stratejinin insafına
bırakılmaz. İhlali look-ahead bias'tır ve backtest'i tamamen değersizleştirir.

**İ3 — Her sonuç benchmark ile birlikte.** Hiçbir performans çıktısı (backtest raporu,
panel, Telegram özeti) benchmark olmadan gösterilmez. Benchmark'lar: BTC al-tut ve
eşit ağırlıklı ilk-10 sepeti. Stratejinin %40 kazandırması, aynı dönemde BTC %60
yapmışsa bir başarısızlıktır.

**İ4 — Maliyetler varsayılan olarak kötümser.** Komisyon ve slipaj her simülasyonda
zorunlu parametredir, varsayılanı sıfır olamaz. Kod sıfır maliyetli bir backtest
koşmayı reddeder (`ErrCostsNotConfigured`).

**İ5 — Emirler idempotenttir.** Her emir istemci tarafından üretilen bir
`ClientOrderID` taşır. Ağ hatasında yeniden deneme çift pozisyon açamaz.

**İ6 — Her karar açıklanabilir.** Üretilen her sinyal, insan tarafından okunabilir bir
gerekçe metni ve karara giren tüm metrik değerlerini taşır. "Model dedi" kabul edilebilir
bir gerekçe değildir.

**İ7 — Devre kesici her zaman açıktır.** Sistem, tanımlı zarar eşiklerinde kendini
otomatik durdurur ve yalnızca manuel müdahaleyle yeniden başlar.

**İ8 — API anahtarı çekim yetkisi almaz.** Hiçbir koşulda. Bkz. Bölüm 13.

---

## 2. Teknoloji seçimleri

| Katman | Seçim | Gerekçe |
|---|---|---|
| Dil | Go 1.22+ | Derlenmiş, tek binary, düşük bellek, mükemmel HTTP/eşzamanlılık, kolay cross-compile |
| Borsa erişimi | `github.com/ccxt/ccxt/go/v4` | Resmi CCXT Go paketi, REST desteği; çok borsalı soyutlama hazır |
| Yedek borsa istemcisi | `github.com/adshao/go-binance/v2` | Binance'e özel, olgun; CCXT'de sorun çıkarsa alternatif |
| Depolama | SQLite (`modernc.org/sqlite`) | Saf Go, cgo yok; tek dosya; sorgulanabilir; bu veri hacmi için fazlasıyla yeterli |
| Ondalık aritmetik | `github.com/shopspring/decimal` | Emir miktarı/fiyatı yuvarlamada float64 hataları kabul edilemez |
| CLI | `github.com/spf13/cobra` | Alt komut yapısı |
| Konfigürasyon | `gopkg.in/yaml.v3` + ortam değişkenleri | Ayarlar dosyada, sırlar ortamda |
| Loglama | `log/slog` (stdlib) | Yapılandırılmış log, ek bağımlılık yok |
| Telegram | `github.com/go-telegram/bot` | Aktif bakımlı; alternatif: `go-telegram-bot-api/telegram-bot-api/v5` |
| Web sunucu | `net/http` (stdlib) | Go 1.22 router'ı yeterli, framework gereksiz |
| Statik varlıklar | `embed` (stdlib) | Panel binary'nin içinde, dağıtım tek dosya |
| Grafik | TradingView Lightweight Charts (yerel olarak vendor'lanmış) | Mum ve çizgi grafikleri için hafif, CDN bağımlılığı yok |
| Test | stdlib `testing` + `testify/require` (opsiyonel) | — |

### Neden Parquet değil SQLite

Önceki planlamada Parquet düşünülmüştü. Go ekosisteminde Parquet ile çalışmak
Python'daki kadar akıcı değil ve bu projenin veri hacmi (~300 sembol × 3 yıl ×
günlük mum ≈ 330 bin satır) SQLite için önemsiz. SQLite ayrıca sorgu, indeks,
transaction ve tek dosyalık yedekleme sağlıyor. Karar: **SQLite**.

### Neden Rust değil

Rust bu iş için de uygun olurdu ancak günlük mumlarla çalışan bir sistemde
performans darboğaz değil. Go'nun geliştirme hızı ve HTTP/JSON ergonomisi burada
daha değerli. Kaynak tüketimi endişesi Go ile zaten karşılanıyor: tipik çalışma
~30-60 MB RAM, boşta ~%0 CPU.

---

## 3. Dizin yapısı

```
swingbot/
├── SPEC.md                       # bu dosya
├── go.mod
├── go.sum
├── Makefile
├── config.example.yaml
├── .env.example
├── .gitignore                    # config.yaml, .env, *.db, data/ MUTLAKA burada
│
├── cmd/
│   └── swingbot/
│       └── main.go               # cobra kök komutu
│
├── internal/
│   ├── domain/                   # saf veri tipleri, bağımlılığı yok
│   │   ├── candle.go
│   │   ├── market.go
│   │   ├── signal.go
│   │   ├── order.go
│   │   ├── position.go
│   │   └── portfolio.go
│   │
│   ├── config/
│   │   └── config.go
│   │
│   ├── store/                    # SQLite katmanı
│   │   ├── store.go
│   │   ├── migrations.go
│   │   ├── candles.go
│   │   ├── proposals.go
│   │   ├── trades.go
│   │   └── runs.go
│   │
│   ├── exchange/                 # borsa soyutlaması
│   │   ├── exchange.go           # Exchange arayüzü
│   │   ├── ccxt.go               # CCXT implementasyonu
│   │   └── ratelimit.go
│   │
│   ├── datafeed/                 # veri toplama ve kalite kontrolü
│   │   ├── fetcher.go
│   │   ├── backfill.go
│   │   └── quality.go
│   │
│   ├── universe/                 # evren filtresi + skorlama
│   │   ├── filter.go
│   │   └── scorer.go
│   │
│   ├── indicator/                # göstergeler, saf fonksiyonlar
│   │   ├── sma.go
│   │   ├── ema.go
│   │   ├── atr.go
│   │   ├── roc.go
│   │   └── rsi.go
│   │
│   ├── strategy/
│   │   ├── strategy.go           # Strategy arayüzü
│   │   ├── momentum.go           # kesitsel momentum
│   │   └── trendfollow.go        # trend takibi + ATR trailing stop
│   │
│   ├── risk/
│   │   ├── sizer.go
│   │   ├── gate.go
│   │   └── breaker.go            # devre kesici
│   │
│   ├── broker/
│   │   ├── broker.go             # Broker arayüzü
│   │   ├── paper.go              # simülasyon (backtest + paper)
│   │   └── live.go               # gerçek emir
│   │
│   ├── backtest/
│   │   ├── engine.go
│   │   ├── costs.go
│   │   ├── metrics.go
│   │   └── walkforward.go
│   │
│   ├── engine/                   # canlı/paper günlük döngü
│   │   ├── daily.go
│   │   └── scheduler.go
│   │
│   ├── notify/
│   │   ├── notifier.go           # Notifier arayüzü
│   │   ├── telegram.go
│   │   └── approval.go           # onay durum makinesi
│   │
│   └── web/
│       ├── server.go
│       ├── api.go                # JSON uçları
│       ├── report.go             # tek dosyalık HTML rapor üreteci
│       └── static/               # embed edilir
│           ├── index.html
│           ├── app.js
│           ├── app.css
│           └── vendor/lightweight-charts.standalone.js
│
├── data/                         # git'e girmez
│   └── swingbot.db
│
└── reports/                      # git'e girmez
    └── backtest_2026-08-15_120000.html
```

**Kural:** `internal/domain` hiçbir şeye bağımlı olmaz. `internal/strategy` yalnızca
`domain` ve `indicator`'a bağımlıdır — store, exchange veya broker'ı **görmez**. Bu,
İ1 (tek kod yolu) ilkesinin derleyici tarafından zorlanmasını sağlar.

---

## 4. Veri modeli

### 4.1 SQLite şeması

```sql
-- Semboller ve borsa filtreleri
CREATE TABLE IF NOT EXISTS markets (
    symbol        TEXT PRIMARY KEY,      -- "BTC/USDT"
    base          TEXT NOT NULL,
    quote         TEXT NOT NULL,
    active        INTEGER NOT NULL,      -- 0/1 : delist edilmişse 0
    tick_size     TEXT NOT NULL,         -- decimal, string olarak saklanır
    step_size     TEXT NOT NULL,
    min_notional  TEXT NOT NULL,
    listed_at     INTEGER,               -- unix ms, bilinmiyorsa NULL
    delisted_at   INTEGER,               -- unix ms, aktifse NULL
    updated_at    INTEGER NOT NULL
);

-- OHLCV
CREATE TABLE IF NOT EXISTS candles (
    symbol       TEXT    NOT NULL,
    timeframe    TEXT    NOT NULL,       -- "1d", "4h"
    open_time    INTEGER NOT NULL,       -- unix ms, UTC, mum AÇILIŞ zamanı
    open         REAL    NOT NULL,
    high         REAL    NOT NULL,
    low          REAL    NOT NULL,
    close        REAL    NOT NULL,
    volume       REAL    NOT NULL,       -- base cinsinden
    quote_volume REAL    NOT NULL,       -- quote cinsinden (likidite filtresi bunu kullanır)
    PRIMARY KEY (symbol, timeframe, open_time)
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS idx_candles_time ON candles(timeframe, open_time);

-- Öneriler ve onay durumu
CREATE TABLE IF NOT EXISTS proposals (
    id            TEXT PRIMARY KEY,      -- ULID/UUID
    created_at    INTEGER NOT NULL,
    as_of         INTEGER NOT NULL,      -- sinyalin ait olduğu mum zamanı
    symbol        TEXT NOT NULL,
    side          TEXT NOT NULL,         -- "long" | "exit"
    strategy      TEXT NOT NULL,
    score         REAL,
    ref_price     REAL NOT NULL,
    stop_price    REAL,
    qty           TEXT NOT NULL,         -- decimal string
    risk_amount   REAL NOT NULL,         -- stop'a kadar riske edilen tutar (quote)
    reason        TEXT NOT NULL,         -- insan okunur gerekçe
    metrics_json  TEXT NOT NULL,         -- karara giren tüm değerler
    status        TEXT NOT NULL,         -- PENDING|APPROVED|REJECTED|EXPIRED|SUBMITTED|FILLED|FAILED
    expires_at    INTEGER NOT NULL,
    decided_at    INTEGER,
    order_id      TEXT
);

CREATE INDEX IF NOT EXISTS idx_proposals_status ON proposals(status, created_at);

-- Emirler
CREATE TABLE IF NOT EXISTS orders (
    id              TEXT PRIMARY KEY,    -- borsa emir id
    client_order_id TEXT UNIQUE NOT NULL,-- idempotency anahtarı
    proposal_id     TEXT,
    symbol          TEXT NOT NULL,
    side            TEXT NOT NULL,
    type            TEXT NOT NULL,       -- "market" | "limit"
    qty             TEXT NOT NULL,
    price           TEXT,
    status          TEXT NOT NULL,
    filled_qty      TEXT NOT NULL,
    avg_price       TEXT,
    fee             TEXT,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    raw_json        TEXT
);

-- Kapanmış işlemler (round-trip)
CREATE TABLE IF NOT EXISTS trades (
    id           TEXT PRIMARY KEY,
    symbol       TEXT NOT NULL,
    strategy     TEXT NOT NULL,
    entry_time   INTEGER NOT NULL,
    entry_price  REAL NOT NULL,
    exit_time    INTEGER,
    exit_price   REAL,
    qty          TEXT NOT NULL,
    fees         REAL NOT NULL,
    pnl_quote    REAL,
    pnl_pct      REAL,
    exit_reason  TEXT,                   -- "stop"|"signal"|"manual"|"breaker"
    mode         TEXT NOT NULL           -- "backtest"|"paper"|"live"
);

-- Günlük portföy anlık görüntüsü (panel ve metrikler için)
CREATE TABLE IF NOT EXISTS equity_snapshots (
    mode        TEXT    NOT NULL,
    ts          INTEGER NOT NULL,
    equity      REAL    NOT NULL,
    cash        REAL    NOT NULL,
    exposure    REAL    NOT NULL,        -- 0..1
    bench_btc   REAL,                    -- aynı başlangıç sermayesiyle BTC al-tut
    bench_top10 REAL,
    PRIMARY KEY (mode, ts)
) WITHOUT ROWID;

-- Backtest koşuları
CREATE TABLE IF NOT EXISTS runs (
    id           TEXT PRIMARY KEY,
    created_at   INTEGER NOT NULL,
    strategy     TEXT NOT NULL,
    params_json  TEXT NOT NULL,
    start_ts     INTEGER NOT NULL,
    end_ts       INTEGER NOT NULL,
    costs_json   TEXT NOT NULL,
    metrics_json TEXT NOT NULL,
    report_path  TEXT,
    git_sha      TEXT                    -- tekrarlanabilirlik
);

-- Sistem durumu (devre kesici vb.)
CREATE TABLE IF NOT EXISTS system_state (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);
```

### 4.2 Zaman kuralları

- Tüm zamanlar **UTC**, **unix milisaniye** olarak saklanır.
- `open_time` mumun **açılış** zamanıdır. Günlük mum için 00:00:00.000 UTC.
- Bir günlük mum ancak `open_time + 24h <= now` ise **kapanmış** sayılır. Kapanmamış
  mum asla stratejiye verilmez. Bu kontrolü `datafeed` katmanında yap, stratejiye
  bırakma.

---

## 5. Çekirdek arayüzler

Bu arayüzler İ1'in (tek kod yolu) taşıyıcısıdır. Backtest ve canlı mod, aynı üst
seviye kodu farklı implementasyonlarla çalıştırır.

### 5.1 domain tipleri

```go
package domain

import (
    "time"
    "github.com/shopspring/decimal"
)

type Candle struct {
    OpenTime    time.Time
    Open        float64
    High        float64
    Low         float64
    Close       float64
    Volume      float64
    QuoteVolume float64
}

type Market struct {
    Symbol      string          // "BTC/USDT"
    Base        string
    Quote       string
    Active      bool
    TickSize    decimal.Decimal // fiyat adımı
    StepSize    decimal.Decimal // miktar adımı
    MinNotional decimal.Decimal // minimum emir tutarı
    ListedAt    time.Time
    DelistedAt  time.Time       // zero ise aktif
}

type Side string

const (
    SideBuy  Side = "buy"
    SideSell Side = "sell"
)

type SignalKind string

const (
    SignalEnter SignalKind = "enter"
    SignalExit  SignalKind = "exit"
    SignalStop  SignalKind = "stop_update"
)

// Signal, stratejinin ürettiği ham niyet. Miktar İÇERMEZ — boyutlandırma risk
// katmanının işidir. Bu ayrım bilinçlidir: strateji sermayeyi bilmez.
type Signal struct {
    AsOf     time.Time  // sinyali üreten mumun OpenTime'ı
    Symbol   string
    Kind     SignalKind
    Score    float64            // sıralama için; enter sinyallerinde anlamlı
    RefPrice float64            // sinyal mumunun kapanışı
    StopPrice float64           // enter için zorunlu
    Reason   string             // İ6: insan okunur gerekçe
    Metrics  map[string]float64 // karara giren tüm değerler
}

type Position struct {
    Symbol     string
    Qty        decimal.Decimal
    EntryPrice float64
    EntryTime  time.Time
    StopPrice  float64
    HighWater  float64 // trailing stop için görülen en yüksek fiyat
    Strategy   string
}

type Portfolio struct {
    Cash      float64
    Positions map[string]Position
    Equity    float64 // Cash + pozisyonların güncel değeri
}

type OrderRequest struct {
    ClientOrderID string          // İ5: idempotency, çağıran üretir
    Symbol        string
    Side          Side
    Type          string          // "market" | "limit"
    Qty           decimal.Decimal
    Price         decimal.Decimal // limit için
}

type Order struct {
    ID            string
    ClientOrderID string
    Symbol        string
    Side          Side
    Type          string
    Qty           decimal.Decimal
    Price         decimal.Decimal
    FilledQty     decimal.Decimal
    AvgPrice      decimal.Decimal
    Fee           decimal.Decimal
    Status        string
    CreatedAt     time.Time
}
```

### 5.2 Exchange

Yalnızca borsayla konuşur. İş mantığı içermez.

```go
package exchange

type Exchange interface {
    Name() string
    FetchMarkets(ctx context.Context) ([]domain.Market, error)
    // FetchOHLCV kapanmamış mumu DÖNEBİLİR; ayıklamak datafeed'in işidir.
    FetchOHLCV(ctx context.Context, symbol, timeframe string, since time.Time, limit int) ([]domain.Candle, error)
    FetchBalance(ctx context.Context) (map[string]decimal.Decimal, error)
    CreateOrder(ctx context.Context, req domain.OrderRequest) (domain.Order, error)
    FetchOrder(ctx context.Context, symbol, id string) (domain.Order, error)
    CancelOrder(ctx context.Context, symbol, id string) error
}
```

**Gereksinimler:**
- Rate limit'e saygılı olmalı (token bucket). Borsanın `429`/`418` yanıtlarında
  üstel geri çekilme (exponential backoff) + jitter uygula.
- Ağ hatalarında yeniden dene; **ancak `CreateOrder` yalnızca `ClientOrderID` ile
  yeniden denenir** ve yeniden denemeden önce `FetchOrder` ile emrin gerçekten
  oluşmadığı doğrulanır.
- Tüm ham yanıtları `orders.raw_json` alanına yaz.

### 5.3 Clock

Backtest ile canlı arasındaki tek zaman farkı burada soyutlanır.

```go
type Clock interface {
    Now() time.Time
    // Sleep, backtest'te anında döner.
    Sleep(d time.Duration)
}
```

### 5.4 Strategy

```go
package strategy

// Series, warmup dahil geçmiş mumları içerir. Son eleman AS-OF mumudur ve
// KAPANMIŞTIR. Motor bunu garanti eder (İ2).
type Input struct {
    AsOf      time.Time
    Series    map[string][]domain.Candle // symbol -> kronolojik mumlar
    Universe  []string                   // bu tarihte işlem yapılabilir semboller
    Portfolio domain.Portfolio
}

type Strategy interface {
    Name() string
    // WarmupBars: en uzun göstergenin ihtiyaç duyduğu mum sayısı.
    // Motor bu kadar mum sağlayamıyorsa sembolü Universe'e koymaz.
    WarmupBars() int
    Params() map[string]any
    Evaluate(in Input) ([]domain.Signal, error)
}
```

**Strateji kodu için katı kurallar:**
- `Input.Series[s]` dizisinin **son elemanından sonrasına** erişemezsin (zaten yok).
- Dış dünyaya erişemezsin: HTTP yok, DB yok, dosya yok, `time.Now()` yok.
- Deterministik olmalısın: aynı `Input` → aynı çıktı. Rastgelelik kullanacaksan
  tohumu (seed) `Params()` içinde açıkça belirt.
- Bu üç kural sayesinde stratejiyi saf birim testiyle doğrulayabilirsin.

### 5.5 Broker

```go
package broker

type Broker interface {
    Mode() string // "backtest" | "paper" | "live"
    Portfolio(ctx context.Context) (domain.Portfolio, error)
    Submit(ctx context.Context, req domain.OrderRequest) (domain.Order, error)
    Cancel(ctx context.Context, symbol, orderID string) error
}
```

- `PaperBroker` hem backtest hem paper trading'de kullanılır. Farkı: backtest'te
  fiyatları geçmiş mumlardan, paper'da canlı veriden alır.
- `LiveBroker` `Exchange`'i sarar, `orders` tablosuna yazar, filtre yuvarlamasını
  uygular (bkz. 6.6).

### 5.6 Notifier

```go
package notify

type Notifier interface {
    ProposeTrade(ctx context.Context, p Proposal) error
    Notify(ctx context.Context, level Level, title, body string) error
    // Approvals, onay/ret olaylarını yayınlar. Motor bunu dinler.
    Approvals() <-chan Decision
}

type Decision struct {
    ProposalID string
    Approved   bool
    At         time.Time
}
```

---

## 6. Modül şartnameleri

### 6.1 datafeed — veri toplama

**Sorumluluk:** Borsadan mumları çekip SQLite'a yazmak, veri kalitesini garanti etmek.

**Backfill (ilk dolum):**
- Her sembol için borsanın izin verdiği en eski tarihten bugüne kadar sayfalı çek.
- Sayfa boyutu genelde 500–1000 mum. Her sayfadan sonra rate limit'e uy.
- İlerlemeyi kalıcı tut; yarıda kesilirse kaldığı yerden devam etsin.
- **En az 3 yıl** veri hedefle. 2022 ayı piyasası, 2023 yatay dönemi ve sonraki
  yükseliş farklı rejimlerdir; strateji üçünde de test edilmelidir.

**Artımlı güncelleme:**
- Her sembol için `MAX(open_time)` sorgula, oradan devam et.
- Son 3 mumu **yeniden çek ve üzerine yaz** — borsalar bazen geçmiş mumu düzeltir.

**Delisted sembol politikası (kritik — survivorship bias):**
- `FetchMarkets` bugün aktif olan sembolleri döner. Geçmişte listelenip sonra
  kaldırılan semboller bu listede yoktur.
- `markets` tablosundan bir sembol kaybolduğunda **satırı silme**; `active = 0`
  ve `delisted_at = now` yaz. Mumları da sakla.
- Backtest evreni her tarih için "o tarihte aktif olan" sembollerden oluşur.
- Projeye sıfırdan başlıyorsan geçmiş delist verisi elinde olmayacak. Bunu kabul et
  ve **rapora bir uyarı satırı olarak bas**: "Bu backtest survivorship bias içerir;
  gerçek performans gösterilenden düşüktür." Zamanla `active=0` kayıtları biriktikçe
  bias azalır.

**Kalite kontrolleri** (`quality.go`) — her güncelleme sonrası çalışır:

| Kontrol | Koşul | Eylem |
|---|---|---|
| Eksik mum | Ardışık `open_time` farkı > timeframe | Logla, sembolü o tarih aralığında evrenden çıkar |
| Sıfır hacim | `volume == 0` | Mumu şüpheli işaretle, evrenden çıkar |
| Geçersiz OHLC | `high < low` veya `close` [low, high] dışında | Hata, mumu reddet |
| Aykırı sıçrama | `abs(close/prev_close - 1) > 0.5` | Uyar, insan incelesin (gerçek olabilir) |
| Gelecek mum | `open_time + tf > now` | Reddet (İ2) |

**Kabul kriteri:** `swingbot data verify` komutu tüm kontrolleri koşup temiz rapor vermeli.

---

### 6.2 universe — evren filtresi ve skorlama

**Filtre** (her tarih için ayrı hesaplanır, geçmişe bakarak değil o tarihteki veriyle):

```
DAHIL ET eğer:
  - market.Quote == "USDT"
  - o tarihte aktif (listed_at <= t < delisted_at)
  - listelenme yaşı >= 180 gün              → yeni coin gürültüsünü ele
  - son 30 günlük medyan quote_volume >= min_volume (varsayılan 5_000_000 USDT)
  - warmup için yeterli mum var
HARIÇ TUT eğer:
  - kaldıraçlı token (sembol "UP", "DOWN", "BULL", "BEAR", "3L", "3S" içeriyor)
  - stablecoin (base bilinen stablecoin listesinde)
  - son 30 günde kalite bayrağı almış
```

**Neden medyan, ortalama değil:** Tek bir pump günü ortalamayı şişirir. Medyan
sürekli likiditeyi ölçer.

**Skorlama:** Skorlayıcı, filtreden geçen sembolleri stratejiye girdi olacak şekilde
sıralar. Skor bileşenlerini ayrı ayrı sakla (`metrics_json`) ki panelde neden o coinin
seçildiğini görebilesin.

Başlangıç skor bileşenleri (her biri kesitsel z-skoru olarak normalize edilir):
- `mom_90` : 90 günlük getiri
- `mom_30` : 30 günlük getiri
- `vol_30` : 30 günlük getiri standart sapması (**negatif ağırlık** — oynaklığı cezalandır)
- `liq` : 30 günlük medyan quote_volume'ün logaritması

```
score = w1*z(mom_90) + w2*z(mom_30) - w3*z(vol_30) + w4*z(liq)
```

Ağırlıklar konfigürasyondan gelir. **Bunları optimize etme dürtüsüne direnç göster** —
bkz. Bölüm 14.

---

### 6.3 indicator — göstergeler

Saf fonksiyonlar. Girdi `[]float64` veya `[]domain.Candle`, çıktı `[]float64`.
Warmup boyunca `NaN` döndür, sıfır değil. Sıfır döndürmek sessiz hataya yol açar.

```go
func SMA(v []float64, n int) []float64
func EMA(v []float64, n int) []float64
func ATR(c []domain.Candle, n int) []float64   // Wilder yumuşatması
func ROC(v []float64, n int) []float64         // (v[i]/v[i-n] - 1)
func RSI(v []float64, n int) []float64
func StdDev(v []float64, n int) []float64
func ZScore(xs []float64) []float64            // kesitsel, tek noktada
```

Her gösterge için birim test: bilinen girdi → elle hesaplanmış çıktı. Bu testleri
atlarsan tüm sistem şüpheli hale gelir.

---

### 6.4 strategy — stratejiler

İki başlangıç stratejisi. **Basit tut.** Karmaşıklık, aşırı uydurmanın ana kaynağıdır.

#### 6.4.1 `momentum` — kesitsel momentum

```
Yeniden dengeleme: haftada bir (Pazartesi mum kapanışı)
Giriş:
  - Evreni score'a göre sırala
  - İlk N'i (varsayılan 5) seç
  - Halihazırda tutulmayan her biri için SignalEnter üret
  - Stop: entry - k * ATR(14), k varsayılan 2.5
Çıkış:
  - Tutulan bir pozisyon ilk 2N (varsayılan 10) dışına düştüyse SignalExit
  - Stop tetiklendiyse SignalExit (motor tetikler, strateji değil)
```

#### 6.4.2 `trendfollow` — trend takibi

```
Giriş (her gün kontrol):
  - close > SMA(200)                        → uzun vadeli trend yukarı
  - close == max(close[-20:])               → 20 günlük kırılım
  - ATR(14)/close  <  max_atr_pct           → aşırı oynak değil
  → SignalEnter, stop = close - 2.5*ATR(14)
Pozisyon yönetimi (her gün):
  - HighWater = max(HighWater, high)
  - yeni_stop = HighWater - 2.5*ATR(14)
  - stop yalnızca YUKARI güncellenir: StopPrice = max(StopPrice, yeni_stop)
  → SignalStop
Çıkış:
  - low <= StopPrice  → motor stop çıkışı uygular
  - close < SMA(50)   → SignalExit
```

**Not:** Stop'un tetiklenmesi motorun işidir, stratejinin değil. Strateji yalnızca
stop **seviyesini** belirler. Bu ayrım, stop mantığının backtest ile canlıda birebir
aynı olmasını garanti eder.

---

### 6.5 risk — boyutlandırma, kapı, devre kesici

#### 6.5.1 Boyutlandırma (`sizer.go`)

```
risk_tutari = equity * risk_per_trade          (varsayılan 0.01 → %1)
stop_mesafesi = entry - stop
ham_qty = risk_tutari / stop_mesafesi
qty = ham_qty, market.StepSize'a AŞAĞI yuvarlanmış
```

Kontroller:
- `qty * entry` > mevcut nakit ise → nakde göre kırp
- `qty * entry` < `market.MinNotional` ise → sinyali **düşür**, gerekçe: `below_min_notional`
- `qty <= 0` ise → düşür
- Tek pozisyonun equity'e oranı `max_position_pct`'i (varsayılan 0.25) aşamaz

**Neden stop mesafesine göre:** Sabit tutarla girmek, oynak coinlerde büyük, sakin
coinlerde küçük risk almak demektir. Stop mesafesine göre boyutlandırma her işlemde
riski eşitler.

#### 6.5.2 Kapı (`gate.go`)

Bir sinyal aşağıdakilerin **hepsinden** geçmeliyse öneriye dönüşür:

| Kural | Varsayılan | Reddetme gerekçesi |
|---|---|---|
| Açık pozisyon sayısı | ≤ 5 | `max_positions` |
| Toplam maruziyet | ≤ %80 equity | `max_exposure` |
| Aynı sembolde açık pozisyon yok | — | `already_open` |
| Son 24 saatte aynı sembolde çıkış yok | — | `cooldown` |
| Devre kesici kapalı | — | `breaker_open` |
| Min notional sağlanıyor | — | `below_min_notional` |
| Nakit yeterli | — | `insufficient_cash` |

Reddedilen sinyaller de kaydedilir. Panelde "neden işlem yapılmadı" görünür olmalı.

#### 6.5.3 Devre kesici (`breaker.go`)

```
AÇIL (işlemleri durdur) eğer:
  - toplam düşüş (peak'ten) >= max_drawdown       (varsayılan 0.15)
  - ardışık zararlı işlem >= max_consecutive_losses (varsayılan 6)
  - günlük kayıp >= max_daily_loss                 (varsayılan 0.05)
  - 24 saatte >= 3 emir hatası
Açıldığında:
  - system_state["breaker"] = "open" + gerekçe + zaman damgası
  - Telegram'a KRİTİK bildirim
  - yeni giriş YOK. Çıkışlar ve stop'lar ÇALIŞMAYA DEVAM EDER.
  - kapanış YALNIZCA manuel: `swingbot breaker reset --confirm`
```

**Neden çıkışlar devam eder:** Devre kesici seni yeni riskten korur, mevcut riskin
içinde kilitlemez.

---

### 6.6 broker

#### 6.6.1 PaperBroker

Emri simüle eder. Doldurma modeli (İ4):

```
Market emri, t+1 gününde:
  temel_fiyat = candle[t+1].Open
  slipaj_yonu = alışta +1, satışta -1
  dolum = temel_fiyat * (1 + slipaj_yonu * slippage_bps/10000)
  komisyon = dolum * qty * fee_rate

Stop çıkışı, t+1 gününde:
  eğer candle[t+1].Low <= stop:
     dolum = min(stop, candle[t+1].Open) * (1 - slippage_bps/10000)
     # Açılış stop'un altındaysa gap olmuştur; stop'tan değil açılıştan dolar.
```

**Bu gap kuralı önemlidir.** Stop'un her zaman tam seviyesinden dolduğunu varsayan
backtest'ler kripto gibi gap'li piyasalarda performansı ciddi biçimde abartır.

Varsayılan maliyetler (konfigürasyondan gelir, sıfır olamaz):
- `fee_rate`: 0.001 (%0.1, çift yön)
- `slippage_bps`: 15 (%0.15) — likit coinler için tutucu, düşük hacimlide artır

#### 6.6.2 LiveBroker

- Emir göndermeden önce fiyat/miktarı `TickSize`/`StepSize`'a yuvarla
  (`decimal.Decimal` ile, **asla float64 ile değil**).
- `ClientOrderID` üret, `orders` tablosuna `PENDING` olarak yaz, **sonra** gönder.
- Yanıt gelmezse: yeniden denemeden önce `FetchOrder(clientOrderID)` ile kontrol et.
- Dolum durumunu takip et; `FILLED` olduğunda `trades` tablosunu güncelle.
- Kısmi dolumları destekle.

---

### 6.7 engine — günlük döngü

```
1.  saat kontrolü: UTC 00:05 (mum kapanışından 5 dk sonra, borsa verisi otursun)
2.  datafeed.Update()                      → yeni mumlar
3.  datafeed.Verify()                      → kalite; başarısızsa dur + bildir
4.  broker.Portfolio()                     → güncel durum
5.  breaker.Check(portfolio)               → gerekiyorsa aç
6.  universe.Build(asOf)                   → o tarihte geçerli evren
7.  strategy.Evaluate(input)               → ham sinyaller
8.  stop ve çıkış sinyallerini ÖNCE işle   → sermayeyi serbest bırak
9.  risk.Gate + risk.Size                  → sinyaller → öneriler
10. proposals tablosuna yaz (PENDING)
11. notify.ProposeTrade(...)               → Telegram
12. onay bekle (timeout: approval_ttl, varsayılan 4 saat)
13. onaylananlar → broker.Submit()
14. equity_snapshots'a yaz
15. günlük özet bildirimi (benchmark ile birlikte — İ3)
```

**Çıkışların girişlerden önce işlenmesi (adım 8) zorunludur.** Aksi halde nakit
yokluğundan geçerli bir giriş sinyalini kaçırırsın; backtest ile canlı ayrışır.

**Yeniden başlatma dayanıklılığı:** Süreç adım 10 ile 13 arasında ölürse, yeniden
başladığında `PENDING` önerileri okuyup kaldığı yerden devam etmeli. Süresi geçmiş
olanları `EXPIRED` işaretle.

---

### 6.8 notify — Telegram ve onay akışı

**Öneri mesajı formatı:**

```
🟢 GİRİŞ ÖNERİSİ · SOL/USDT

Strateji   trendfollow
Referans   142.30 USDT (14 Ağu kapanış)
Stop       128.07 USDT (-10.0%)
Miktar     12.6 SOL ≈ 1793 USDT
Risk       180 USDT (equity'nin %1.0'i)

Gerekçe
20 günlük kırılım. Fiyat SMA200'ün %18 üzerinde.
ATR(14)/fiyat = %4.0 (limit %8). Evren skoru: 2. / 47

Portföy sonrası
Pozisyon 3/5 · Maruziyet %62 · Nakit 6810 USDT

Geçerlilik: 4 saat

[ ✅ Onayla ]  [ ❌ Reddet ]  [ 📊 Detay ]
```

**Onay durum makinesi:**

```
PENDING ──onayla──> APPROVED ──emir──> SUBMITTED ──dolum──> FILLED
   │                                        │
   ├──reddet──> REJECTED                    └──hata──> FAILED
   └──zaman aşımı──> EXPIRED
```

**Güvenlik:** Bot yalnızca konfigürasyondaki `telegram.allowed_chat_id`'den gelen
komutları kabul eder. Başka chat'ten gelen her şey sessizce yok sayılır ve loglanır.

**Bildirilecek diğer olaylar:** stop tetiklenmesi, devre kesici, veri kalitesi hatası,
emir hatası, günlük özet.

---

## 7. Görsel katman

### 7.1 Yerel panel

Binary `swingbot serve` ile `localhost:8080`'de bir panel açar. Statik varlıklar
`embed` ile binary'nin içindedir; dağıtım hâlâ tek dosya.

**Sayfalar:**

| Yol | İçerik |
|---|---|
| `/` | Genel bakış: equity eğrisi + benchmark, açık pozi