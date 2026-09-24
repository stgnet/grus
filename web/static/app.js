// The site works without JavaScript. This file only makes a few things nicer:
//
// 1. Photos are shrunk in the browser to at most 2048px before upload, so a
//    12 MB phone photo costs a few hundred KB of cellular data instead. The
//    server re-processes every photo anyway (orientation, metadata, size), so
//    this is purely about upload size.
// 2. Delete and Remove buttons ask "are you sure?".
// 3. Search: quick answers (fact cards) are fetched after the page shows its
//    plain results, and "Not what I was looking for" posts in place.
// 4. While writing a post, earlier posts that may answer it appear under
//    the title, each with a "Link to this" box.
(function () {
  "use strict";
  var MAX = 2048;

  function shrink(file) {
    if (!file.type || file.type.indexOf("image/") !== 0 || !window.createImageBitmap) {
      return Promise.resolve(file);
    }
    // imageOrientation: "from-image" applies the EXIF rotation while drawing.
    return createImageBitmap(file, { imageOrientation: "from-image" }).then(function (bmp) {
      var scale = Math.min(1, MAX / Math.max(bmp.width, bmp.height));
      if (scale === 1 && file.size < 2 * 1024 * 1024) {
        return file; // already small enough
      }
      var c = document.createElement("canvas");
      c.width = Math.round(bmp.width * scale);
      c.height = Math.round(bmp.height * scale);
      c.getContext("2d").drawImage(bmp, 0, 0, c.width, c.height);
      return new Promise(function (resolve) {
        c.toBlob(function (b) { resolve(b ? new File([b], "photo.jpg", { type: "image/jpeg" }) : file); }, "image/jpeg", 0.9);
      });
    }).catch(function () { return file; });
  }

  function post(url, fields) {
    var body = new URLSearchParams();
    Object.keys(fields).forEach(function (k) { body.append(k, fields[k]); });
    return fetch(url, { method: "POST", body: body, credentials: "same-origin" }).then(function (r) {
      if (!r.ok) { throw new Error(r.status); }
      return r.text();
    });
  }

  // Quick answers. The server answers within its own time limit with
  // either the cards or one plain line; the page never shows a spinner
  // longer than that, and never an error.
  var cards = document.getElementById("cards");
  if (cards && window.fetch) {
    var soft = "Quick answers aren't available right now. These posts match your search.";
    var done = false;
    var fallback = setTimeout(function () {
      if (!done) { done = true; cards.innerHTML = '<p class="soft">' + soft + "</p>"; cards.hidden = false; }
    }, 10000);
    cards.innerHTML = '<p class="muted">Looking for quick answers…</p>';
    cards.hidden = false;
    post("/ask", { q: cards.getAttribute("data-q"), prev: cards.getAttribute("data-prev") || "" }).then(function (html) {
      if (done) { return; }
      done = true;
      clearTimeout(fallback);
      cards.innerHTML = html; // server-rendered and escaped
      cards.hidden = html.trim() === "";
    }).catch(function () {
      if (done) { return; }
      done = true;
      clearTimeout(fallback);
      cards.innerHTML = '<p class="soft">' + soft + "</p>";
    });
  }

  // "These earlier posts may answer this", under the title while writing.
  var similar = document.getElementById("similar");
  var source = similar && document.getElementById(similar.getAttribute("data-source"));
  if (similar && source && window.fetch) {
    var timer = null, last = "";
    var refresh = function () {
      var q = source.value.trim();
      if (q === last) { return; }
      last = q;
      if (q.length < 8) { similar.innerHTML = ""; return; }
      // Keep what's already ticked across refreshes.
      var ticked = {};
      similar.querySelectorAll("input[name=link]:checked").forEach(function (i) { ticked[i.value] = true; });
      fetch("/similar?q=" + encodeURIComponent(q), { credentials: "same-origin" }).then(function (r) { return r.text(); }).then(function (html) {
        if (source.value.trim() !== q) { return; }
        similar.innerHTML = html;
        similar.querySelectorAll("input[name=link]").forEach(function (i) { if (ticked[i.value]) { i.checked = true; } });
      }).catch(function () {});
    };
    source.addEventListener("input", function () { clearTimeout(timer); timer = setTimeout(refresh, 400); });
    refresh();
  }

  document.addEventListener("submit", function (e) {
    var form = e.target;
    if (form.classList.contains("confirm") && !window.confirm(form.getAttribute("data-confirm") || "Are you sure?")) {
      e.preventDefault();
      return;
    }
    if (form.classList.contains("fetch-swap") && window.fetch) {
      // Small forms whose answer replaces them in place.
      e.preventDefault();
      var fields = {};
      new FormData(form).forEach(function (v, k) { fields[k] = v; });
      post(form.action, fields).then(function (html) { form.outerHTML = html; }).catch(function () { form.submit(); });
      return;
    }
    if (!form.classList.contains("resize") || !window.fetch || !window.FormData) {
      return;
    }
    var inputs = form.querySelectorAll('input[type="file"]');
    var any = false;
    inputs.forEach(function (i) { if (i.files && i.files.length) { any = true; } });
    if (!any) {
      return;
    }
    e.preventDefault();
    var button = form.querySelector("button");
    if (button) { button.disabled = true; button.textContent = "Uploading…"; }
    var data = new FormData(form);
    var jobs = [];
    inputs.forEach(function (input) {
      data.delete(input.name);
      Array.prototype.forEach.call(input.files, function (f) {
        jobs.push(shrink(f).then(function (s) { return [input.name, s]; }));
      });
    });
    Promise.all(jobs).then(function (files) {
      files.forEach(function (nf) { data.append(nf[0], nf[1], nf[1].name || "photo.jpg"); });
      return fetch(form.action, { method: "POST", body: data, credentials: "same-origin" });
    }).then(function (resp) {
      if (resp.redirected) {
        window.location = resp.url;
        return;
      }
      return resp.text().then(function (html) {
        document.open(); document.write(html); document.close();
      });
    }).catch(function () {
      form.submit(); // fall back to a plain upload
    });
  });
})();
