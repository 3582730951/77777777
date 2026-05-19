from __future__ import annotations

from scripts.list_bitbrowser_profiles import extract_profiles, list_bitbrowser_profiles, profile_summary


def test_extract_profiles_from_common_shapes():
    profile = {"id": "profile-1", "name": "qa"}

    assert extract_profiles({"data": {"list": [profile]}}) == [profile]
    assert extract_profiles({"data": {"items": [profile]}}) == [profile]
    assert extract_profiles({"list": [profile]}) == [profile]


def test_profile_summary_prefers_id_and_common_fields():
    summary = profile_summary(
        {
            "id": "profile-1",
            "seq": 8,
            "name": "QA Account",
            "remark": "checkout",
            "groupId": "group-1",
            "status": 1,
        }
    )

    assert summary == {
        "id": "profile-1",
        "seq": "8",
        "name": "QA Account",
        "remark": "checkout",
        "groupId": "group-1",
        "status": "1",
    }


def test_list_bitbrowser_profiles_posts_filter_payload(monkeypatch):
    captured = {}

    def fake_post_json(url, payload, timeout):
        captured["url"] = url
        captured["payload"] = payload
        captured["timeout"] = timeout
        return {"data": {"list": [{"id": "profile-1"}]}}

    monkeypatch.setattr("scripts.list_bitbrowser_profiles._post_json", fake_post_json)

    result = list_bitbrowser_profiles(
        api_base="127.0.0.1:54345",
        page=1,
        page_size=50,
        name="qa",
        remark="test",
        group_id="group-1",
        timeout=10,
    )

    assert captured["url"] == "http://127.0.0.1:54345/browser/list"
    assert captured["payload"] == {
        "page": 1,
        "pageSize": 50,
        "name": "qa",
        "remark": "test",
        "groupId": "group-1",
    }
    assert captured["timeout"] == 10
    assert result == {"data": {"list": [{"id": "profile-1"}]}}
