-- Mulatschak v1 (Salzburg) – 33-card William Tell deck
-- All 6s removed EXCEPT Weli (Bells-6), which is permanent trump.
-- Specials: Hearts trump doubles scores; Acorns trump = no drop-out (no staying home).
-- Bidding min=2; announcing 1 is only legal if trump=Hearts (and then Hearts is trump).
-- Auto-Mulatschak: if hand holds {A(trump), Weli, K(trump), Q(trump), J(trump)} → skip bidding; must lead trump Ace.

local M = {}

function M.spec()
  return {
    name = "mulatschak",
    seats = {min=2, max=5, default=3},
    deck = {
      -- suits = {"A"/*Acorns*/, "L"/*Leaves*/, "H"/*Hearts*/, "B"/*Bells*/},
      suits = {"S", "C", "H", "D"},
      ranks = {"A","K","Q","J","10","9","8","7"},
      include_weli = true,  -- D-6
    },
    rules = {
      hearts_double = true,
      acorns_no_dropout = true,
      bidding_min = 2,
      one_only_if_hearts = true,
      auto_mulatschak = true,
    }
  }
end

-- Utility: check auto-mulatschak predicate for a hand
-- hand: array of {suit=..., rank=...} (rank "A","K","Q","J","10","9","8","7" or "WELI")
function M.is_auto_mulatschak(hand)
  -- Count by suit
  local suits = {A={},L={},H={},B={}}
  local weli = false
  for _,c in ipairs(hand) do
    if c.rank == "WELI" then weli = true else
      (suits[c.suit])[c.rank] = true
    end
  end
  if not weli then return false end
  local tops = {"A","K","Q","J"} -- {"A","K","O","U"}
  for s,_ in pairs(suits) do
    local ok = true
    for _,r in ipairs(tops) do
      if not suits[s][r] then ok=false; break end
    end
    if ok then return true, s end
  end
  return false
end

-- Stubs: the host (Go) will call these; flesh out in iterations
function M.legal_moves(state, seat)
  return {}
end

function M.apply_move(state, seat, move)
  return state, { {type="noop"} }
end

function M.score(state)
  return { } -- implement scoring after base flow
end

return M